-- Global user identities. These queries are deliberately not
-- tenant-scoped: a user exists once across tenants. Tenant-facing reads go
-- through membership.sql, which always filters by tenant_id.

-- name: GetUserByID :one
SELECT sqlc.embed(u),
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM users u
WHERE u.id = $1 AND u.status <> 'deleted';

-- name: GetUserByEmail :one
-- The address is stored normalized (chk_users_email_normalized), so the
-- lookup is an equality on idx_users_email_live.
SELECT sqlc.embed(u),
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM users u
WHERE u.email = $1 AND u.status <> 'deleted';

-- name: GetUserByIdentity :one
SELECT sqlc.embed(u),
       EXISTS (SELECT 1 FROM user_passwords p WHERE p.user_id = u.id)::boolean AS has_password
FROM user_identities i
JOIN users u ON u.id = i.user_id
WHERE i.provider = $1 AND i.subject = $2 AND u.status <> 'deleted';

-- name: CreateUser :one
INSERT INTO users (email, name, email_verified_at)
VALUES (@email, @name, CASE WHEN @email_verified::boolean THEN NOW() END)
RETURNING *;

-- name: CreateUserPassword :exec
INSERT INTO user_passwords (user_id, password_hash)
VALUES ($1, $2);

-- name: GetUserPasswordHash :one
SELECT p.password_hash
FROM user_passwords p
JOIN users u ON u.id = p.user_id
WHERE p.user_id = $1 AND u.status <> 'deleted';

-- name: UpsertUserPassword :exec
INSERT INTO user_passwords (user_id, password_hash)
VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE
SET password_hash = EXCLUDED.password_hash,
    updated_at = NOW();

-- name: RehashUserPassword :execrows
-- A successful sign-in may upgrade a hash written under older cost parameters
-- without a credential change (no token_version bump). It is conditional on
-- the hash that verified, so a concurrent password change wins and the stale
-- upgrade never overwrites the new credential.
UPDATE user_passwords
SET password_hash = sqlc.arg(new_hash)
WHERE user_id = sqlc.arg(user_id) AND password_hash = sqlc.arg(current_hash);

-- name: RecordUserCredentialChange :one
-- A password set/reset ends every access token and session of the user in
-- every tenant and invalidates reset links issued before it.
UPDATE users
SET token_version = token_version + 1,
    credentials_changed_at = NOW()
WHERE id = $1 AND status <> 'deleted'
RETURNING *;

-- name: CreateUserIdentity :exec
INSERT INTO user_identities (user_id, provider, subject, email)
VALUES ($1, $2, $3, $4);

-- name: UpdateUserProfile :one
UPDATE users
SET name = @name,
    -- Omit email when the caller did not supply one, so a name-only update
    -- cannot clobber a concurrent email change.
    email = CASE WHEN @update_email::boolean THEN @email ELSE email END,
    -- Changing the address invalidates any prior verification of the old one,
    -- pending reset links, and every access/refresh token for the identity.
    email_verified_at = CASE
        WHEN @update_email::boolean AND email IS DISTINCT FROM @email THEN NULL
        ELSE email_verified_at
    END,
    token_version = token_version + CASE
        WHEN @update_email::boolean AND email IS DISTINCT FROM @email THEN 1 ELSE 0
    END,
    credentials_changed_at = CASE
        WHEN @update_email::boolean AND email IS DISTINCT FROM @email THEN NOW()
        ELSE credentials_changed_at
    END
WHERE id = @id AND status <> 'deleted'
RETURNING *;

-- name: VerifyUserEmail :one
UPDATE users SET email_verified_at = COALESCE(email_verified_at, NOW())
WHERE id = $1 AND status <> 'deleted' AND email = $2
RETURNING *;

-- name: RevokeUserTokens :one
UPDATE users
SET token_version = token_version + 1
WHERE id = $1 AND status <> 'deleted'
RETURNING *;

-- name: MarkUserDeleted :one
UPDATE users
SET status = 'deleted', deleted_at = NOW(),
    token_version = token_version + 1, credentials_changed_at = NOW()
WHERE id = $1 AND status <> 'deleted'
RETURNING *;
