-- name: CreateSession :one
-- A session is only created for an active member whose token version still
-- equals the one the caller authenticated at. This closes the bcrypt-length
-- window where a password change commits after the password check.
INSERT INTO sessions (tenant_id, user_id, token_version, expires_at, user_agent, ip, amr)
SELECT m.tenant_id, m.user_id, u.token_version, @expires_at, @user_agent, @ip, @amr
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id AND m.user_id = @user_id
  AND m.status = 'active' AND u.status = 'active'
  AND u.token_version = @token_version
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions
WHERE tenant_id = $1 AND id = $2;

-- name: ListLiveSessionsCursor :many
SELECT * FROM sessions
WHERE tenant_id = @tenant_id::bigint AND user_id = @user_id::bigint
  AND revoked_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: RevokeSession :execrows
UPDATE sessions
SET revoked_at = NOW(), revoked_reason = @reason
WHERE tenant_id = @tenant_id AND user_id = @user_id AND id = @id
  AND revoked_at IS NULL;

-- name: RevokeTenantUserSessions :exec
UPDATE sessions
SET revoked_at = NOW(), revoked_reason = @reason
WHERE tenant_id = @tenant_id AND user_id = @user_id AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :exec
-- Deliberately not tenant-scoped: global credential events (password, email,
-- logout-all, account deletion) end the user's sessions in every tenant.
UPDATE sessions
SET revoked_at = NOW(), revoked_reason = @reason
WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: RevokeOtherUserSessions :exec
-- An MFA change ends every other sign-in (every tenant) but keeps the session
-- that performed it.
UPDATE sessions
SET revoked_at = NOW(), revoked_reason = @reason
WHERE user_id = @user_id AND id <> @keep_id AND revoked_at IS NULL;

-- name: RevokeSessionByRefreshToken :one
-- Possession-based (logout and refresh-token reuse): presenting the opaque
-- token is sufficient to revoke its session, even without X-Tenant-Slug. It
-- cannot read or mint anything, so it does not weaken tenant isolation.
UPDATE sessions s
SET revoked_at = NOW(), revoked_reason = @reason
FROM refresh_tokens r
WHERE r.token_hash = @token_hash
  AND s.tenant_id = r.tenant_id AND s.id = r.session_id
  AND s.revoked_at IS NULL
RETURNING s.user_id;

-- name: TouchSession :exec
UPDATE sessions
SET last_used_at = NOW(), user_agent = @user_agent, ip = @ip
WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetSessionState :one
-- The access-token validator's single lookup: the session (with its tenant
-- and revocation), the membership status and effective permissions (tenant
-- roles plus platform assignments), the user's token version and status, and
-- the tenant's status. role_keys is ordered owner, admin, member.
SELECT s.tenant_id, s.revoked_at, s.expires_at, m.status AS membership_status,
       u.token_version, u.status AS user_status, t.status AS tenant_status,
       COALESCE((
           SELECT array_agg(r.key ORDER BY
               CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key)
           FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = s.tenant_id AND mr.user_id = s.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
       ), '{}'::text[])::text[] AS role_keys,
       COALESCE((
           SELECT array_agg(DISTINCT p.permission_key)
           FROM (
               SELECT rp.permission_key
               FROM membership_roles mr
               JOIN roles r ON r.id = mr.role_id
               JOIN role_permissions rp ON rp.role_id = r.id
               WHERE mr.tenant_id = s.tenant_id AND mr.user_id = s.user_id AND r.scope = 'tenant'
               UNION
               SELECT rp.permission_key
               FROM platform_role_assignments pra
               JOIN roles r ON r.id = pra.role_id
               JOIN role_permissions rp ON rp.role_id = r.id
               WHERE pra.user_id = s.user_id AND r.scope = 'platform'
           ) p
       ), '{}'::text[])::text[] AS permission_keys,
       EXISTS (
           SELECT 1 FROM platform_role_assignments pra
           JOIN roles r ON r.id = pra.role_id
           WHERE pra.user_id = s.user_id AND r.scope = 'platform'
       ) AS platform
FROM sessions s
JOIN memberships m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id
JOIN users u ON u.id = s.user_id
JOIN tenants t ON t.id = s.tenant_id
WHERE s.id = $1 AND s.user_id = $2;

-- name: PurgeStaleSessions :execrows
-- Maintenance purge across every tenant: sessions revoked longer ago than
-- the retention window, and sessions whose refresh tokens have all been
-- purged (the family can never be used again). One bounded batch per call.
WITH batch AS (
    SELECT s.id FROM sessions s
    WHERE (s.revoked_at IS NOT NULL AND s.revoked_at < @revoked_before::timestamptz)
       OR (s.last_used_at < @idle_before::timestamptz
           AND NOT EXISTS (
               SELECT 1 FROM refresh_tokens r
               WHERE r.tenant_id = s.tenant_id AND r.session_id = s.id
           ))
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM sessions AS t
USING batch
WHERE t.id = batch.id;
