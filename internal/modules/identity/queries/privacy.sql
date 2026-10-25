-- Data exports and privacy erasure. Every export query is tenant/user scoped;
-- ListPurgeableDataExports is the deliberate cross-tenant retention pass.

-- name: CreateDataExport :one
INSERT INTO data_exports (tenant_id, user_id)
VALUES (@tenant_id::bigint, @user_id::bigint)
RETURNING *;

-- name: GetDataExport :one
SELECT * FROM data_exports
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint;

-- name: ListDataExportsCursor :many
SELECT * FROM data_exports
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: ListDataExportsForUser :many
SELECT * FROM data_exports
WHERE tenant_id = @tenant_id::bigint AND user_id = @user_id::bigint
ORDER BY id;

-- name: MarkDataExportRunning :execrows
UPDATE data_exports
SET status = 'running', updated_at = NOW()
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND status IN ('queued', 'failed', 'running');

-- name: MarkDataExportReady :execrows
UPDATE data_exports
SET status = 'ready', storage_path = @storage_path::varchar,
    expires_at = @expires_at::timestamptz, updated_at = NOW()
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND status = 'running';

-- name: MarkDataExportFailed :exec
UPDATE data_exports
SET status = 'failed', updated_at = NOW()
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND status IN ('queued', 'running', 'failed');

-- name: ExpireDataExportsForUser :many
UPDATE data_exports
SET status = 'expired', expires_at = COALESCE(expires_at, NOW()), updated_at = NOW()
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND status <> 'expired'
RETURNING *;

-- name: DeleteDataExportsForUser :exec
DELETE FROM data_exports
WHERE tenant_id = @tenant_id::bigint AND user_id = @user_id::bigint;

-- name: ListPurgeableDataExports :many
SELECT * FROM data_exports
WHERE status = 'expired'
   OR (expires_at IS NOT NULL AND expires_at <= NOW())
   -- A crashed/timed-out export or a failed build must not stay active
   -- forever: the job timeout is 10 minutes, so a day is stale.
   OR (status IN ('queued', 'running', 'failed') AND updated_at <= NOW() - INTERVAL '1 day')
ORDER BY id
LIMIT @batch_size::int;

-- name: DeleteDataExport :exec
DELETE FROM data_exports WHERE id = @id::bigint;

-- name: ListUserSessionsForPrivacy :many
SELECT * FROM sessions
WHERE tenant_id = @tenant_id::bigint AND user_id = @user_id::bigint
ORDER BY created_at DESC, id DESC;

-- name: DeleteTenantUserSessions :exec
DELETE FROM sessions
WHERE tenant_id = @tenant_id::bigint AND user_id = @user_id::bigint;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = @user_id::bigint;

-- name: AnonymizeUser :one
-- Also upgrades an account deleted by the older self-service path, which left
-- the email, name, and credentials behind. A fully erased row matches nothing.
UPDATE users
SET email = @email::varchar,
    name = '',
    email_verified_at = NULL,
    status = 'deleted',
    deleted_at = COALESCE(deleted_at, NOW()),
    token_version = token_version + 1,
    credentials_changed_at = NOW()
WHERE id = @id::bigint
  AND (status <> 'deleted' OR email <> @email::varchar OR name <> ''
       OR email_verified_at IS NOT NULL)
RETURNING *;

-- name: DeleteUserPasswords :exec
DELETE FROM user_passwords WHERE user_id = @user_id::bigint;

-- name: DeleteUserIdentities :exec
DELETE FROM user_identities WHERE user_id = @user_id::bigint;
