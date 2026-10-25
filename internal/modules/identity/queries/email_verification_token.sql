-- Verification tokens are per user and bound to the address they were sent to.

-- name: CreateEmailVerificationToken :one
INSERT INTO email_verification_tokens (user_id, email, token_hash, expires_at)
SELECT u.id, @email, @token_hash, @expires_at
FROM users u
WHERE u.id = @user_id AND u.status <> 'deleted'
ON CONFLICT (user_id) DO UPDATE
SET email = EXCLUDED.email,
    token_hash = EXCLUDED.token_hash,
    expires_at = EXCLUDED.expires_at,
    created_at = NOW()
RETURNING *;

-- name: ConsumeEmailVerificationToken :one
DELETE FROM email_verification_tokens
WHERE token_hash = $1
RETURNING *;

-- name: DeleteEmailVerificationTokensByUserID :exec
DELETE FROM email_verification_tokens
WHERE user_id = $1;

-- name: PurgeExpiredEmailVerificationTokens :execrows
-- Maintenance purge; see PurgeExpiredPasswordResetTokens.
WITH batch AS (
    SELECT id FROM email_verification_tokens
    WHERE expires_at < now()
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM email_verification_tokens AS t
USING batch
WHERE t.id = batch.id;
