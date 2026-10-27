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
WHERE m.tenant_id = @tenant_id::uuid
  AND m.status = 'active' AND u.status = 'active'
  AND (m.created_at, m.user_id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
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
WHERE m.tenant_id = @tenant_id::uuid
  AND (m.created_at, m.user_id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY m.created_at DESC, m.user_id DESC
LIMIT @row_limit::int;

-- name: ListMembersFilteredCursor :many
-- Admin view with optional membership-status and role-key filters.
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
WHERE m.tenant_id = @tenant_id::uuid
  AND (sqlc.narg('status')::text IS NULL OR m.status = sqlc.narg('status')::text)
  AND (sqlc.narg('role')::text IS NULL OR EXISTS (
      SELECT 1 FROM membership_roles mr
      JOIN roles r ON r.id = mr.role_id
      WHERE mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id AND r.key = sqlc.narg('role')::text
  ))
  AND (m.created_at, m.user_id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
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
-- here: a suspended membership matches no row, and neither does a removed
-- membership that kept its ban timestamp. An erasure reservation is cleared:
-- a re-invited member starts a fresh membership in the tenant.
UPDATE memberships
SET status = 'active', removed_at = NULL, erasure_requested_at = NULL
WHERE tenant_id = $1 AND user_id = $2 AND status = 'removed'
  AND banned_at IS NULL
RETURNING *;

-- name: SuspendMembershipForErasure :execrows
-- Reserves a membership for erasure inside the request transaction: the
-- member can no longer act in the tenant and a concurrent last-owner check
-- sees one fewer active owner. banned_at/banned_email_hash are deliberately
-- untouched: this is not a ban.
UPDATE memberships
SET status = 'suspended',
    erasure_requested_at = NOW(),
    removed_at = NULL,
    credentials_revoked_at = clock_timestamp()
WHERE tenant_id = $1 AND user_id = $2
  AND status <> 'removed';

-- name: BanMembership :one
UPDATE memberships AS m
SET status = 'suspended', banned_at = NOW(),
    -- Keyed marker (HMAC-SHA256 under a key derived from the data-encryption
    -- key set), computed in Go: a database reader cannot confirm or
    -- dictionary-recover the address. The key id records which key wrote it.
    banned_email_hash = @banned_email_hash::bytea,
    banned_email_hash_kid = @banned_email_hash_kid::text,
    -- Kept by UnbanMembership, so API keys issued before the ban stay dead.
    -- clock_timestamp(), not NOW(): an API-key insert holding the membership
    -- row FOR SHARE (LockMemberPrincipal) makes this UPDATE wait, and that
    -- key's created_at (its transaction start) must still compare at or
    -- before the revocation, which NOW() does not guarantee.
    credentials_revoked_at = clock_timestamp()
WHERE m.tenant_id = $1 AND m.user_id = $2 AND m.status = 'active'
RETURNING m.*;

-- name: UnbanMembership :one
-- Only a real ban is lifted. A membership suspended for a pending erasure
-- matches no row, so an unban can never resurrect it.
UPDATE memberships
SET status = 'active', banned_at = NULL, banned_email_hash = NULL,
    banned_email_hash_kid = NULL
WHERE tenant_id = $1 AND user_id = $2 AND status = 'suspended'
  AND banned_at IS NOT NULL AND erasure_requested_at IS NULL
RETURNING *;

-- name: RemoveMembership :one
UPDATE memberships AS m
SET status = 'removed', removed_at = NOW(),
    -- A ban (and its keyed marker) survives removal and erasure, so the
    -- address cannot re-register in this tenant. A ban recorded before the
    -- keyed marker existed gets one backfilled by Go (the caller passes it);
    -- an erasure reservation (no banned_at) must not create one.
    banned_at = m.banned_at,
    banned_email_hash = CASE
        WHEN m.banned_email_hash IS NOT NULL THEN m.banned_email_hash
        ELSE NULLIF(@banned_email_hash::bytea, ''::bytea)
    END,
    banned_email_hash_kid = CASE
        WHEN m.banned_email_hash IS NOT NULL THEN m.banned_email_hash_kid
        ELSE NULLIF(@banned_email_hash_kid::text, '')
    END,
    credentials_revoked_at = clock_timestamp()
WHERE m.tenant_id = $1 AND m.user_id = $2 AND m.status <> 'removed'
RETURNING m.*;

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
SELECT pg_advisory_xact_lock(hashtextextended('identity.last_owner:' || @tenant_id::uuid, 0));

-- name: MembershipStats :one
SELECT
    count(*) FILTER (WHERE m.status = 'active' AND u.status = 'active')::bigint AS active_members,
    count(*) FILTER (WHERE m.status <> 'active' OR u.status <> 'active')::bigint AS inactive_members
FROM memberships m
JOIN users u ON u.id = m.user_id
WHERE m.tenant_id = $1;

-- name: EmailBannedInTenant :one
-- Public registration, invitation acceptance, sign-in account creation, and
-- email changes refuse an address whose tenant membership retains a matching
-- keyed ban marker. The caller passes the marker under the active key and
-- every configured previous key, so a ban survives a rotation while the key
-- that wrote it remains configured, and keeps blocking after erasure removed
-- the membership and anonymized the user. Served by
-- idx_memberships_tenant_banned_email_hash (bans are few per tenant).
SELECT EXISTS (
    SELECT 1 FROM memberships m
    WHERE m.tenant_id = @tenant_id::uuid
      AND m.banned_email_hash = ANY(@email_hashes::bytea[])
)::boolean;

-- name: CountBannedMarkers :many
-- Startup check: retained ban markers per key id across every tenant, so an
-- operator is warned that markers written with a no-longer-configured key can
-- no longer match (they cannot be re-hashed after erasure).
SELECT COALESCE(banned_email_hash_kid, '')::text AS banned_email_hash_kid,
       count(*)::bigint AS bans
FROM memberships
WHERE banned_email_hash IS NOT NULL
GROUP BY banned_email_hash_kid
ORDER BY banned_email_hash_kid;

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
       m.credentials_revoked_at,
       CASE
           WHEN t.status = 'active'
                OR (t.status = 'trialing' AND t.trial_ends_at > NOW())
           THEN 'active'
           ELSE 'suspended'
       END AS tenant_status
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
       m.credentials_revoked_at,
       CASE
           WHEN t.status = 'active'
                OR (t.status = 'trialing' AND t.trial_ends_at > NOW())
           THEN 'active'
           ELSE 'suspended'
       END AS tenant_status
FROM memberships m
JOIN users u ON u.id = m.user_id
JOIN tenants t ON t.id = m.tenant_id
WHERE m.tenant_id = $1 AND m.user_id = $2
  AND m.status = 'active' AND u.status = 'active'
FOR SHARE OF m;

-- name: ListTenantMembershipUserIDs :many
-- The tenant purge walks every membership, including removed ones, in user-id
-- batches so a large tenant never loads in one statement. The tenant purge
-- then runs the privacy erasers for each user before deleting the memberships.
SELECT user_id FROM memberships
WHERE tenant_id = @tenant_id::uuid
  AND user_id > @after_user_id::uuid
ORDER BY user_id
LIMIT @row_limit::int;

-- name: DeleteTenantMembership :execrows
-- Tenant purge: removes one membership right after that member's erasers
-- succeeded. Its sessions, refresh tokens, and role assignments cascade; the
-- per-member invitations are deleted first to release the RESTRICT FKs.
DELETE FROM memberships
WHERE tenant_id = @tenant_id::uuid AND user_id = @user_id::uuid;

-- name: DeleteTenantMembershipsBatch :execrows
-- Final sweep after every module purger and invitation delete. A deleted
-- tenant is not operational, so the last-owner trigger permits the delete.
-- One bounded page per statement.
DELETE FROM memberships
WHERE id IN (
    SELECT id FROM memberships
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);

-- name: TouchMembershipIfUnchanged :execrows
-- The optimistic-concurrency gate for the memberships ETag. Two writers that
-- read the same updated_at cannot both win: the first commits a new value and
-- the second re-evaluates the predicate to 0 rows.
UPDATE memberships
SET updated_at = NOW()
WHERE tenant_id = @tenant_id
  AND user_id = @user_id
  AND updated_at = @expected_updated_at;
