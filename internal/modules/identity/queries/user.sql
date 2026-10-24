-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: GetUserSessionState :one
-- Deliberately keyed only by user id: the session validator compares the
-- token's tenant claim with the row's tenant and rejects mismatches and
-- suspended tenants. This is not used for any tenant-facing data read.
SELECT u.role, u.token_version, u.tenant_id, t.status
FROM users u
JOIN tenants t ON t.id = u.tenant_id
WHERE u.id = $1 AND u.deleted_at IS NULL;

-- name: GetUserPrincipal :one
-- API-key authentication needs the owner's current role and the owning
-- tenant's status in one lookup, so a suspended tenant's keys stop working.
-- credentials_revoked_at lets the verifier reject keys created before a ban.
SELECT u.id, u.tenant_id, u.email, u.name, u.role, u.credentials_revoked_at, t.status AS tenant_status
FROM users u
JOIN tenants t ON t.id = u.tenant_id
WHERE u.id = $1 AND u.tenant_id = $2 AND u.deleted_at IS NULL;

-- name: LockUserPrincipal :one
-- GetUserPrincipal under a FOR SHARE lock on the user row, for a caller's
-- transaction that writes a credential for this owner (API-key create and
-- rotate). A concurrent ban/delete waits for that transaction, and one that
-- committed first makes this return no row, so no key is minted for it.
SELECT u.id, u.tenant_id, u.email, u.name, u.role, u.credentials_revoked_at, t.status AS tenant_status
FROM users u
JOIN tenants t ON t.id = u.tenant_id
WHERE u.id = $1 AND u.tenant_id = $2 AND u.deleted_at IS NULL
FOR SHARE OF u;

-- name: GetUserByEmail :one
SELECT * FROM users
WHERE tenant_id = $1 AND lower(email) = lower($2) AND deleted_at IS NULL;

-- name: GetBannedUserByEmail :one
SELECT * FROM users
WHERE tenant_id = $1 AND lower(email) = lower($2) AND banned_at IS NOT NULL
LIMIT 1;

-- name: GetDeletedUserByEmail :one
-- Startup admin seed only (one query per boot): finds a banned or
-- self-deleted account so the seed never resurrects it.
SELECT * FROM users
WHERE tenant_id = $1 AND lower(email) = lower($2) AND deleted_at IS NOT NULL
LIMIT 1;

-- name: ListUsers :many
SELECT * FROM users
WHERE tenant_id = $1 AND deleted_at IS NULL
ORDER BY id LIMIT $2 OFFSET $3;

-- name: CountUsers :one
SELECT count(*) FROM users
WHERE tenant_id = $1 AND deleted_at IS NULL;

-- name: CreateUser :one
INSERT INTO users (tenant_id, email, password_hash, name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: CreateAdminUser :one
-- Startup admin seed: the role and verification are set in the same INSERT,
-- so a crash can never leave a half-seeded non-admin account behind.
INSERT INTO users (tenant_id, email, password_hash, name, role, email_verified_at)
VALUES ($1, $2, $3, $4, 'admin', NOW())
RETURNING *;

-- name: UpdateUser :one
UPDATE users
SET name = $1,
    -- Omit email when the caller did not supply one, so a name-only update
    -- cannot clobber a concurrent email change.
    email = CASE WHEN @update_email::boolean THEN $2 ELSE email END,
    -- Changing the address invalidates any prior verification of the old one,
    -- pending reset links, and every access/refresh token for the old identity.
    email_verified_at = CASE
        WHEN @update_email::boolean AND email IS DISTINCT FROM $2 THEN NULL
        ELSE email_verified_at
    END,
    token_version = token_version + CASE
        WHEN @update_email::boolean AND email IS DISTINCT FROM $2 THEN 1 ELSE 0
    END,
    password_changed_at = CASE
        WHEN @update_email::boolean AND email IS DISTINCT FROM $2 THEN NOW()
        ELSE password_changed_at
    END,
    updated_at = NOW()
WHERE id = $3 AND tenant_id = $4 AND deleted_at IS NULL
RETURNING *;

-- name: DeleteUser :one
UPDATE users
SET deleted_at = NOW(), updated_at = NOW(), token_version = token_version + 1,
    password_changed_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: BanUser :one
UPDATE users
SET deleted_at = NOW(), banned_at = NOW(), updated_at = NOW(),
    token_version = token_version + 1, password_changed_at = NOW(),
    -- Kept by RestoreUser, so API keys issued before the ban stay dead.
    -- clock_timestamp(), not NOW(): an API-key insert holding the owner row
    -- FOR SHARE (LockUserPrincipal) makes this UPDATE wait, and that key's
    -- created_at (its transaction start) must still compare at or before the
    -- revocation, which NOW() (this transaction's start) does not guarantee.
    credentials_revoked_at = clock_timestamp()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreUser :one
-- Unban only reverses a ban. A self-deleted account (deleted_at set,
-- banned_at NULL) is not restorable through this path.
UPDATE users
SET deleted_at = NULL, banned_at = NULL, updated_at = NOW(),
    token_version = token_version + 1, password_changed_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NOT NULL AND banned_at IS NOT NULL
RETURNING *;

-- name: CountDeletedUsers :one
SELECT count(*) FROM users
WHERE tenant_id = $1 AND deleted_at IS NOT NULL;

-- name: GetUserByGoogleID :one
SELECT * FROM users
WHERE tenant_id = $1 AND google_id = $2 AND deleted_at IS NULL;

-- name: CreateOAuthUser :one
INSERT INTO users (tenant_id, email, name, google_id, auth_provider, email_verified_at)
VALUES ($1, $2, $3, $4, $5, NOW())
RETURNING *;

-- name: LinkGoogleAccount :one
-- Never replaces a different Google identity: zero rows means the account is
-- already linked to another Google account (or is no longer active).
UPDATE users SET google_id = @google_id, auth_provider = 'google', updated_at = NOW()
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
  AND (google_id IS NULL OR google_id = @google_id)
RETURNING *;

-- name: UpdateUserPassword :one
UPDATE users
SET password_hash = $1,
    updated_at = NOW(),
    token_version = token_version + 1,
    password_changed_at = NOW()
WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL
RETURNING *;

-- name: VerifyUserEmail :one
UPDATE users SET email_verified_at = NOW(), updated_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND email = $3
RETURNING *;

-- name: RevokeUserSessions :one
UPDATE users
SET token_version = token_version + 1, updated_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateUserRole :one
UPDATE users
SET role = $1, updated_at = NOW(), token_version = token_version + 1
WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL
RETURNING *;

-- name: CountActiveAdmins :one
SELECT count(*) FROM users
WHERE tenant_id = $1 AND role = 'admin' AND deleted_at IS NULL;

-- name: LockTenantAdminChanges :exec
-- Serializes admin demotion/ban/delete per tenant for the rest of the
-- transaction. It must run before the target row is updated: the
-- protect_last_admin trigger locks the other admins' rows, and without this
-- lock two concurrent demotions each hold their own row and wait on the
-- other's (deadlock).
SELECT pg_advisory_xact_lock(hashtextextended('identity.last_admin', @tenant_id::bigint));

-- name: AdminUserStats :one
SELECT
    count(*) FILTER (WHERE deleted_at IS NULL)::bigint AS active_users,
    count(*) FILTER (WHERE deleted_at IS NOT NULL)::bigint AS deleted_users
FROM users
WHERE tenant_id = $1;

-- name: AdminListUsers :many
SELECT * FROM users
WHERE tenant_id = $1
ORDER BY id LIMIT $2 OFFSET $3;

-- name: AdminCountUsers :one
SELECT count(*) FROM users
WHERE tenant_id = $1;

-- name: ListUsersCursor :many
SELECT * FROM users
WHERE tenant_id = @tenant_id::bigint
  AND deleted_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: AdminListUsersCursor :many
SELECT * FROM users
WHERE tenant_id = @tenant_id::bigint
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;
