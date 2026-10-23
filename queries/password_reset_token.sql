-- name: CreatePasswordResetToken :one
INSERT INTO password_reset_tokens (user_id, token, expires_at)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
SET token = EXCLUDED.token,
    expires_at = EXCLUDED.expires_at,
    created_at = NOW()
RETURNING *;

-- name: GetPasswordResetTokenByToken :one
SELECT * FROM password_reset_tokens WHERE token = $1;

-- name: ConsumePasswordResetToken :one
DELETE FROM password_reset_tokens WHERE token = $1 RETURNING *;

-- name: GetPasswordResetTokenByTokenForUpdate :one
SELECT * FROM password_reset_tokens WHERE token = $1 FOR UPDATE;

-- name: DeletePasswordResetToken :exec
DELETE FROM password_reset_tokens WHERE token = $1;

-- name: DeletePasswordResetTokensByUserID :exec
DELETE FROM password_reset_tokens WHERE user_id = $1;
