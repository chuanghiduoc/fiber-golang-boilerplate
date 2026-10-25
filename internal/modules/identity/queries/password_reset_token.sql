-- Reset tokens are per user (the password is a global credential).

-- name: CreatePasswordResetToken :one
-- The reset job looks the account up by address outside this transaction.
-- Re-checking the address here under a FOR SHARE row lock means an email
-- change committed in between, or still in flight, yields zero rows instead
-- of a valid link mailed to the old address.
INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
SELECT u.id, @token_hash, @expires_at
FROM users u
WHERE u.id = @user_id
  AND u.status = 'active'
  AND u.email = @email
FOR SHARE OF u
ON CONFLICT (user_id) DO UPDATE
SET token_hash = EXCLUDED.token_hash,
    expires_at = EXCLUDED.expires_at,
    created_at = NOW()
RETURNING *;

-- name: GetPasswordResetTokenByHash :one
SELECT * FROM password_reset_tokens
WHERE token_hash = $1;

-- name: ConsumePasswordResetToken :one
DELETE FROM password_reset_tokens
WHERE token_hash = $1
RETURNING *;

-- name: DeletePasswordResetToken :exec
DELETE FROM password_reset_tokens
WHERE token_hash = $1;

-- name: DeletePasswordResetTokensByUserID :exec
DELETE FROM password_reset_tokens
WHERE user_id = $1;

-- name: PurgeExpiredPasswordResetTokens :execrows
-- Maintenance purge; see PurgeExpiredRefreshTokens. The one-row-per-user
-- table stays small enough to purge without an expires_at index.
WITH batch AS (
    SELECT id FROM password_reset_tokens
    WHERE expires_at < now()
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM password_reset_tokens AS t
USING batch
WHERE t.id = batch.id;
