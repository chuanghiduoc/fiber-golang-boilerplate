-- Privacy export/erasure. The key hash is deliberately never selected.

-- name: ListAPIKeysForPrivacy :many
SELECT id, tenant_id, user_id, name, prefix, expires_at, revoked_at,
       rotated_at, rotation_grace_until, last_used_at, created_at, updated_at,
       ARRAY(SELECT s.permission_key FROM api_key_scopes s
             WHERE s.api_key_id = api_keys.id
             ORDER BY s.permission_key COLLATE "C")::text[] AS scopes
FROM api_keys
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
ORDER BY id
LIMIT @row_limit::int;

-- name: DeleteAPIKeysForPrivacy :execrows
DELETE FROM api_keys
WHERE tenant_id = @tenant_id::uuid AND user_id = @user_id::uuid;
