-- name: CreateEmailVerificationToken :one
INSERT INTO email_verification_tokens (tenant_id, user_id, email, token, expires_at)
SELECT u.tenant_id, $1, $2, $3, $4
FROM users u
WHERE u.id = $1 AND u.tenant_id = $5 AND u.deleted_at IS NULL
ON CONFLICT (user_id) DO UPDATE
SET tenant_id = EXCLUDED.tenant_id,
    email = EXCLUDED.email,
    token = EXCLUDED.token,
    expires_at = EXCLUDED.expires_at,
    created_at = NOW()
RETURNING *;

-- name: GetEmailVerificationTokenByToken :one
SELECT * FROM email_verification_tokens
WHERE token = $1 AND tenant_id = $2;

-- name: ConsumeEmailVerificationToken :one
DELETE FROM email_verification_tokens
WHERE token = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteEmailVerificationToken :exec
DELETE FROM email_verification_tokens
WHERE token = $1 AND tenant_id = $2;

-- name: DeleteEmailVerificationTokensByUserID :exec
DELETE FROM email_verification_tokens
WHERE user_id = $1 AND tenant_id = $2;
