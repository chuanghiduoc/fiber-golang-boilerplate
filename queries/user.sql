-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower($1) AND deleted_at IS NULL;

-- name: ListUsers :many
SELECT * FROM users WHERE deleted_at IS NULL ORDER BY id LIMIT $1 OFFSET $2;

-- name: CountUsers :one
SELECT count(*) FROM users WHERE deleted_at IS NULL;

-- name: CreateUser :one
INSERT INTO users (email, password_hash, name)
VALUES ($1, $2, $3)
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
WHERE id = $3 AND deleted_at IS NULL
RETURNING *;

-- name: DeleteUser :one
UPDATE users
SET deleted_at = NOW(), updated_at = NOW(), token_version = token_version + 1,
    password_changed_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreUser :one
UPDATE users
SET deleted_at = NULL, updated_at = NOW(), token_version = token_version + 1,
    password_changed_at = NOW()
WHERE id = $1 AND deleted_at IS NOT NULL
RETURNING *;

-- name: ListDeletedUsers :many
SELECT * FROM users WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC LIMIT $1 OFFSET $2;

-- name: CountDeletedUsers :one
SELECT count(*) FROM users WHERE deleted_at IS NOT NULL;

-- name: GetUserByGoogleID :one
SELECT * FROM users WHERE google_id = $1 AND deleted_at IS NULL;

-- name: CreateOAuthUser :one
INSERT INTO users (email, name, google_id, auth_provider, email_verified_at)
VALUES ($1, $2, $3, $4, NOW())
RETURNING *;

-- name: LinkGoogleAccount :one
UPDATE users SET google_id = $1, auth_provider = 'google', updated_at = NOW()
WHERE id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateUserPassword :one
UPDATE users
SET password_hash = $1,
    updated_at = NOW(),
    token_version = token_version + 1,
    password_changed_at = NOW()
WHERE id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: VerifyUserEmail :one
UPDATE users SET email_verified_at = NOW(), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL AND email = $2
RETURNING *;

-- name: UpdateUserRole :one
UPDATE users
SET role = $1, updated_at = NOW(), token_version = token_version + 1
WHERE id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: CountActiveAdmins :one
SELECT count(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL;

-- name: AdminListUsers :many
SELECT * FROM users ORDER BY id LIMIT $1 OFFSET $2;

-- name: AdminCountUsers :one
SELECT count(*) FROM users;

-- name: ListUsersCursor :many
SELECT * FROM users
WHERE deleted_at IS NULL
  AND (NOT @has_cursor::boolean OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: AdminListUsersCursor :many
SELECT * FROM users
WHERE (NOT @has_cursor::boolean OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;
