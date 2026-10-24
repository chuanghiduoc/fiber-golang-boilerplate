-- name: CreatePasswordResetToken :one
-- The reset job looks the account up by address outside this transaction.
-- Re-checking the address here (same normalization as GetUserByEmail) under a
-- FOR SHARE row lock means an email change committed in between, or still in
-- flight, yields zero rows instead of a valid link mailed to the old address.
INSERT INTO password_reset_tokens (tenant_id, user_id, token, expires_at)
SELECT u.tenant_id, @user_id, @token, @expires_at
FROM users u
WHERE u.id = @user_id
  AND u.tenant_id = @tenant_id
  AND u.deleted_at IS NULL
  AND lower(u.email) = lower(@email)
FOR SHARE OF u
ON CONFLICT (tenant_id, user_id) DO UPDATE
SET token = EXCLUDED.token,
    expires_at = EXCLUDED.expires_at,
    created_at = NOW()
RETURNING *;

-- name: GetPasswordResetTokenByToken :one
SELECT * FROM password_reset_tokens
WHERE token = $1 AND tenant_id = $2;

-- name: ConsumePasswordResetToken :one
DELETE FROM password_reset_tokens
WHERE token = $1 AND tenant_id = $2
RETURNING *;

-- name: GetPasswordResetTokenByTokenForUpdate :one
SELECT * FROM password_reset_tokens
WHERE token = $1 AND tenant_id = $2
FOR UPDATE;

-- name: DeletePasswordResetToken :exec
DELETE FROM password_reset_tokens
WHERE token = $1 AND tenant_id = $2;

-- name: DeletePasswordResetTokensByUserID :exec
DELETE FROM password_reset_tokens
WHERE user_id = $1 AND tenant_id = $2;

-- name: PurgeExpiredPasswordResetTokens :execrows
-- Maintenance purge across every tenant (an expired token is dead in any
-- tenant). One bounded batch per call; SKIP LOCKED avoids waiting on rows a
-- concurrent purge or reset already holds.
WITH batch AS (
    SELECT id FROM password_reset_tokens
    WHERE expires_at < now()
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM password_reset_tokens AS t
USING batch
WHERE t.id = batch.id;
