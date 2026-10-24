-- name: CreateAPIKey :one
INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAPIKeyByID :one
SELECT * FROM api_keys
WHERE id = $1 AND tenant_id = $2;

-- name: GetAPIKeyByHash :one
-- The hash is globally unique. This lookup intentionally has no tenant
-- predicate because the verifier learns the key's tenant from the row before
-- the request can be scoped.
SELECT * FROM api_keys
WHERE key_hash = $1;

-- name: UpdateAPIKey :one
UPDATE api_keys
SET name = $1,
    scopes = $2,
    updated_at = NOW()
WHERE id = $3 AND tenant_id = $4 AND revoked_at IS NULL
RETURNING *;

-- name: RevokeAPIKey :one
UPDATE api_keys
SET revoked_at = NOW(),
    updated_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL
RETURNING *;

-- name: MarkAPIKeyRotated :one
UPDATE api_keys
SET rotated_at = NOW(),
    rotation_grace_until = NOW() + make_interval(secs => @grace_seconds::int),
    updated_at = NOW()
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND revoked_at IS NULL
  AND rotated_at IS NULL
RETURNING *;

-- name: TouchAPIKeyLastUsed :exec
UPDATE api_keys
SET last_used_at = $2
WHERE id = $1;

-- name: ListAPIKeysCursor :many
SELECT * FROM api_keys
WHERE tenant_id = @tenant_id::bigint
  AND (NOT @has_cursor::boolean OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;
