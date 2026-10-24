-- name: CreatePasswordResetToken :one
INSERT INTO password_reset_tokens (tenant_id, user_id, token, expires_at)
SELECT u.tenant_id, $1, $2, $3
FROM users u
WHERE u.id = $1 AND u.tenant_id = $4 AND u.deleted_at IS NULL
ON CONFLICT (user_id) DO UPDATE
SET tenant_id = EXCLUDED.tenant_id,
    token = EXCLUDED.token,
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
