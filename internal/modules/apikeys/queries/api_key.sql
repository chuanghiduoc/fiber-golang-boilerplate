-- Scopes live in api_key_scopes. Every query returns them as one sorted
-- text[] column (byte order, matching the service's sort.Slice), so a key and
-- its scopes are always read in one statement. Writes change the key and its
-- scope rows in a single statement as well (data-modifying CTEs), so they are
-- atomic even without an enclosing transaction.

-- name: CreateAPIKey :one
WITH key AS (
    INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, expires_at)
    VALUES (@tenant_id, @user_id, @name, @prefix, @key_hash, @expires_at)
    RETURNING *
), granted AS (
    INSERT INTO api_key_scopes (api_key_id, permission_key)
    SELECT key.id, scope
    FROM key, unnest(@scopes::text[]) AS scope
)
SELECT key.*, @scopes::text[] AS scopes
FROM key;

-- name: GetAPIKeyByID :one
SELECT api_keys.*,
       ARRAY(SELECT s.permission_key FROM api_key_scopes s
             WHERE s.api_key_id = api_keys.id
             ORDER BY s.permission_key COLLATE "C")::text[] AS scopes
FROM api_keys
WHERE id = $1 AND tenant_id = $2;

-- name: GetAPIKeyByHash :one
-- The hash is globally unique. This lookup intentionally has no tenant
-- predicate because the verifier learns the key's tenant from the row before
-- the request can be scoped.
SELECT api_keys.*,
       ARRAY(SELECT s.permission_key FROM api_key_scopes s
             WHERE s.api_key_id = api_keys.id
             ORDER BY s.permission_key COLLATE "C")::text[] AS scopes
FROM api_keys
WHERE key_hash = $1;

-- name: UpdateAPIKey :one
-- Replaces the scope set: rows outside @scopes are deleted and missing ones
-- inserted. The two scope statements never touch the same row, so they can
-- run in one statement despite sharing a snapshot.
WITH key AS (
    UPDATE api_keys
    SET name = @name,
        updated_at = NOW()
    WHERE id = @id AND tenant_id = @tenant_id AND revoked_at IS NULL
    RETURNING *
), revoked AS (
    DELETE FROM api_key_scopes AS s
    USING key
    WHERE s.api_key_id = key.id
      AND NOT (s.permission_key = ANY(@scopes::text[]))
), granted AS (
    INSERT INTO api_key_scopes (api_key_id, permission_key)
    SELECT key.id, scope
    FROM key, unnest(@scopes::text[]) AS scope
    ON CONFLICT (api_key_id, permission_key) DO NOTHING
)
SELECT key.*, @scopes::text[] AS scopes
FROM key;

-- name: RevokeAPIKey :one
UPDATE api_keys
SET revoked_at = NOW(),
    updated_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL
RETURNING *,
          ARRAY(SELECT s.permission_key FROM api_key_scopes s
                WHERE s.api_key_id = api_keys.id
                ORDER BY s.permission_key COLLATE "C")::text[] AS scopes;

-- name: MarkAPIKeyRotated :one
UPDATE api_keys
SET rotated_at = NOW(),
    rotation_grace_until = NOW() + make_interval(secs => @grace_seconds::int),
    updated_at = NOW()
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND revoked_at IS NULL
  AND rotated_at IS NULL
RETURNING *,
          ARRAY(SELECT s.permission_key FROM api_key_scopes s
                WHERE s.api_key_id = api_keys.id
                ORDER BY s.permission_key COLLATE "C")::text[] AS scopes;

-- name: TouchAPIKeyLastUsed :exec
UPDATE api_keys
SET last_used_at = $2
WHERE id = $1;

-- name: ListAPIKeysCursor :many
SELECT api_keys.*,
       ARRAY(SELECT s.permission_key FROM api_key_scopes s
             WHERE s.api_key_id = api_keys.id
             ORDER BY s.permission_key COLLATE "C")::text[] AS scopes
FROM api_keys
WHERE tenant_id = @tenant_id::uuid
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: ListAPIKeysByUserCursor :many
-- One member's keys for /me/api-keys and /members/{memberId}/api-keys. The
-- tenant predicate keeps RLS and the explicit scope aligned; user_id narrows
-- the page without ever exposing another member's rows.
SELECT api_keys.*,
       ARRAY(SELECT s.permission_key FROM api_key_scopes s
             WHERE s.api_key_id = api_keys.id
             ORDER BY s.permission_key COLLATE "C")::text[] AS scopes
FROM api_keys
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;
