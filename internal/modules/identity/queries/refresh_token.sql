-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (user_id, token, expires_at, token_version)
SELECT $1, $2, $3, u.token_version
FROM users u
WHERE u.id = $1
  AND u.deleted_at IS NULL
  -- Compare with the version the caller authenticated/consumed. This closes
  -- the bcrypt-length window where a password change could commit after the
  -- password check but before this INSERT.
  AND u.token_version = $4
RETURNING *;

-- name: GetRefreshTokenByToken :one
SELECT * FROM refresh_tokens WHERE token = $1;

-- name: ConsumeRefreshToken :one
DELETE FROM refresh_tokens WHERE token = $1 RETURNING *;

-- name: DeleteRefreshToken :exec
DELETE FROM refresh_tokens WHERE token = $1;

-- name: DeleteRefreshTokensByUserID :exec
DELETE FROM refresh_tokens WHERE user_id = $1;
