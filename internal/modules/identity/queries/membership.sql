-- Tenant membership queries. Every query filters by tenant_id except
-- ListUserMemberships, which global account deletion uses to visit each of
-- the user's tenants. role/roles are derived from membership_roles; the
-- highest system role (owner > admin > member) is the compatibility role.

-- name: GetMember :one
SELECT sqlc.embed(u), sqlc.embed(m),
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(r.key ORDER BY r.key)
           FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
       ), '{}'::text[])::text[] AS roles,
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1 AND m.user_id = $2;

-- name: GetActiveMember :one
SELECT sqlc.embed(u), sqlc.embed(m),
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(r.key ORDER BY r.key)
           FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
       ), '{}'::text[])::text[] AS roles,
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1 AND m.user_id = $2
  AND m.status = 'active' AND u.status = 'active';

-- name: ListActiveMembersCursor :many
SELECT sqlc.embed(u), sqlc.embed(m),
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(r.key ORDER BY r.key)
           FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
       ), '{}'::text[])::text[] AS roles,
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id::bigint
  AND m.status = 'active' AND u.status = 'active'
  AND (m.created_at, m.user_id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY m.created_at DESC, m.user_id DESC
LIMIT @row_limit::int;

-- name: ListMembersCursor :many
-- Admin view: every membership state, plus globally deleted users.
SELECT sqlc.embed(u), sqlc.embed(m),
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(r.key ORDER BY r.key)
           FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
       ), '{}'::text[])::text[] AS roles,
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = @tenant_id::bigint
  AND (m.created_at, m.user_id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY m.created_at DESC, m.user_id DESC
LIMIT @row_limit::int;

-- name: ListUserMemberships :many
SELECT sqlc.embed(m),
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(r.key ORDER BY r.key)
           FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
       ), '{}'::text[])::text[] AS roles
FROM memberships m
WHERE m.user_id = $1
ORDER BY m.tenant_id;

-- name: CreateMembership :one
INSERT INTO memberships (tenant_id, user_id)
VALUES ($1, $2)
RETURNING *;

-- name: ReactivateMembership :one
-- Re-joining after removal (invitation acceptance). A ban is never lifted
-- here: a suspended membership matches no row.
UPDATE memberships
SET status = 'active', removed_at = NULL
WHERE tenant_id = $1 AND user_id = $2 AND status = 'removed'
RETURNING *;

-- name: BanMembership :one
UPDATE memberships
SET status = 'suspended', banned_at = NOW(),
    -- Kept by UnbanMembership, so API keys issued before the ban stay dead.
    -- clock_timestamp(), not NOW(): an API-key insert holding the membership
    -- row FOR SHARE (LockMemberPrincipal) makes this UPDATE wait, and that
    -- key's created_at (its transaction start) must still compare at or
    -- before the revocation, which NOW() does not guarantee.
    credentials_revoked_at = clock_timestamp()
WHERE tenant_id = $1 AND user_id = $2 AND status = 'active'
RETURNING *;

-- name: UnbanMembership :one
UPDATE memberships
SET status = 'active', banned_at = NULL
WHERE tenant_id = $1 AND user_id = $2 AND status = 'suspended'
RETURNING *;

-- name: RemoveMembership :one
UPDATE memberships
SET status = 'removed', removed_at = NOW(), banned_at = NULL,
    credentials_revoked_at = clock_timestamp()
WHERE tenant_id = $1 AND user_id = $2 AND status <> 'removed'
RETURNING *;

-- name: LockMembership :execrows
-- Serializes rank checks and role changes for one membership: every mutation
-- that needs the target's current highest role locks this row first, and
-- role replacement takes the same lock, so a concurrent change cannot race
-- the rank comparison.
SELECT 1 FROM memberships
WHERE tenant_id = @tenant_id AND user_id = @user_id
FOR UPDATE;

-- name: CountActiveOwners :one
SELECT count(*) FROM membership_roles mr
JOIN memberships m ON m.tenant_id = mr.tenant_id AND m.user_id = mr.user_id
JOIN users u ON u.id = m.user_id
JOIN roles r ON r.id = mr.role_id
WHERE mr.tenant_id = $1
  AND r.tenant_id IS NULL AND r.scope = 'tenant' AND r.key = 'owner'
  AND m.status = 'active' AND u.status = 'active';

-- name: LockTenantOwnerChanges :exec
-- Serializes owner demotion/ban/removal per tenant for the rest of the
-- transaction. It uses the same advisory key as the protect_last_owner
-- trigger, so concurrent changes cannot both observe a surviving owner.
SELECT pg_advisory_xact_lock(hashtextextended('identity.last_owner', @tenant_id::bigint));

-- name: MembershipStats :one
SELECT
    count(*) FILTER (WHERE m.status = 'active' AND u.status = 'active')::bigint AS active_members,
    count(*) FILTER (WHERE m.status <> 'active' OR u.status <> 'active')::bigint AS inactive_members
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1;

-- name: EmailBannedInTenant :one
-- Public registration, invitation acceptance, and email changes refuse an
-- address whose account (live or self-deleted) is banned in the tenant, so a
-- ban survives the banned user deleting and re-creating the account. Served
-- by idx_memberships_tenant_suspended (bans are few per tenant).
SELECT EXISTS (
    SELECT 1 FROM memberships m
    JOIN users u ON u.id = m.user_id
    WHERE m.tenant_id = $1 AND m.status = 'suspended' AND u.email = $2
)::boolean;

-- name: GetMemberPrincipal :one
-- API-key authentication needs the owner's current role and effective
-- permissions plus the owning tenant's status in one lookup, so a suspended
-- tenant's keys stop working. credentials_revoked_at lets the verifier reject
-- keys created before a ban.
SELECT u.id, m.tenant_id, u.email, u.name,
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(DISTINCT p.permission_key)
           FROM (
               SELECT rp.permission_key
               FROM membership_roles mr
               JOIN roles r ON r.id = mr.role_id
               JOIN role_permissions rp ON rp.role_id = r.id
               WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
               UNION
               SELECT rp.permission_key
               FROM platform_role_assignments pra
               JOIN roles r ON r.id = pra.role_id
               JOIN role_permissions rp ON rp.role_id = r.id
               WHERE pra.user_id = m.user_id AND r.scope = 'platform'
           ) p
       ), '{}'::text[])::text[] AS permissions,
       EXISTS (
           SELECT 1 FROM platform_role_assignments pra
           JOIN roles r ON r.id = pra.role_id
           WHERE pra.user_id = m.user_id AND r.scope = 'platform'
       ) AS platform,
       m.credentials_revoked_at, t.status AS tenant_status
FROM memberships m
JOIN users u ON u.id = m.user_id
JOIN tenants t ON t.id = m.tenant_id
WHERE m.tenant_id = $1 AND m.user_id = $2
  AND m.status = 'active' AND u.status = 'active';

-- name: LockMemberPrincipal :one
-- GetMemberPrincipal under a FOR SHARE lock on the membership row, for a
-- caller's transaction that writes a credential for this owner (API-key
-- create and rotate). A concurrent ban/removal waits for that transaction,
-- and one that committed first makes this return no row, so no key is minted.
SELECT u.id, m.tenant_id, u.email, u.name,
       COALESCE((
           SELECT r.key FROM membership_roles mr
           JOIN roles r ON r.id = mr.role_id
           WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
             AND r.scope = 'tenant' AND r.tenant_id IS NULL
           ORDER BY CASE r.key WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, r.key
           LIMIT 1
       ), 'member')::text AS role,
       COALESCE((
           SELECT array_agg(DISTINCT p.permission_key)
           FROM (
               SELECT rp.permission_key
               FROM membership_roles mr
               JOIN roles r ON r.id = mr.role_id
               JOIN role_permissions rp ON rp.role_id = r.id
               WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.scope = 'tenant'
               UNION
               SELECT rp.permission_key
               FROM platform_role_assignments pra
               JOIN roles r ON r.id = pra.role_id
               JOIN role_permissions rp ON rp.role_id = r.id
               WHERE pra.user_id = m.user_id AND r.scope = 'platform'
           ) p
       ), '{}'::text[])::text[] AS permissions,
       EXISTS (
           SELECT 1 FROM platform_role_assignments pra
           JOIN roles r ON r.id = pra.role_id
           WHERE pra.user_id = m.user_id AND r.scope = 'platform'
       ) AS platform,
       m.credentials_revoked_at, t.status AS tenant_status
FROM memberships m
JOIN users u ON u.id = m.user_id
JOIN tenants t ON t.id = m.tenant_id
WHERE m.tenant_id = $1 AND m.user_id = $2
  AND m.status = 'active' AND u.status = 'active'
FOR SHARE OF m;
