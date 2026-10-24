-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (tenant_id, user_id, token, expires_at, token_version)
SELECT u.tenant_id, $1, $2, $3, u.token_version
FROM users u
WHERE u.id = $1
  AND u.tenant_id = $5
  AND u.deleted_at IS NULL
  -- Compare with the version the caller authenticated/consumed. This closes
  -- the bcrypt-length window where a password change could commit after the
  -- password check but before this INSERT.
  AND u.token_version = $4
RETURNING *;

-- name: GetRefreshTokenByToken :one
SELECT * FROM refresh_tokens
WHERE token = $1 AND tenant_id = $2;

-- name: ConsumeRefreshToken :one
DELETE FROM refresh_tokens
WHERE token = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteRefreshToken :exec
DELETE FROM refresh_tokens
WHERE token = $1 AND tenant_id = $2;

-- name: DeleteRefreshTokenByPossession :exec
-- Logout is possession-based: presenting the opaque token is sufficient to
-- revoke it, even if the caller omits X-Tenant-Slug. Possession cannot be used
-- to read or mint anything, so this does not weaken tenant isolation.
DELETE FROM refresh_tokens
WHERE token = $1;

-- name: DeleteRefreshTokensByUserID :exec
DELETE FROM refresh_tokens
WHERE user_id = $1 AND tenant_id = $2;

-- name: PurgeExpiredRefreshTokens :execrows
-- Maintenance purge across every tenant; see PurgeExpiredPasswordResetTokens.
-- Served by idx_refresh_tokens_expires_at.
WITH batch AS (
    SELECT id FROM refresh_tokens
    WHERE expires_at < now()
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM refresh_tokens AS t
USING batch
WHERE t.id = batch.id;
