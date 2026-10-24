-- name: CreateEmailVerificationToken :one
INSERT INTO email_verification_tokens (user_id, email, token, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE
SET email = EXCLUDED.email,
    token = EXCLUDED.token,
    expires_at = EXCLUDED.expires_at,
    created_at = NOW()
RETURNING *;

-- name: GetEmailVerificationTokenByToken :one
SELECT * FROM email_verification_tokens WHERE token = $1;

-- name: ConsumeEmailVerificationToken :one
DELETE FROM email_verification_tokens WHERE token = $1 RETURNING *;

-- name: DeleteEmailVerificationToken :exec
DELETE FROM email_verification_tokens WHERE token = $1;

-- name: DeleteEmailVerificationTokensByUserID :exec
DELETE FROM email_verification_tokens WHERE user_id = $1;
