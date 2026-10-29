-- name: CreateFile :one
INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetFileByID :one
SELECT * FROM files
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: GetFileByIDIncludingDeleted :one
SELECT * FROM files
WHERE id = $1 AND tenant_id = $2;

-- name: DeleteFile :one
UPDATE files SET deleted_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreFile :one
UPDATE files SET deleted_at = NULL
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NOT NULL
  AND purge_started_at IS NULL
RETURNING *;

-- name: AdminFileStats :one
SELECT
    count(*)::bigint AS total_files,
    COALESCE(SUM(size), 0)::bigint AS total_file_size
FROM files
WHERE tenant_id = $1 AND deleted_at IS NULL;

-- name: ListFilesByUserCursor :many
SELECT * FROM files
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND (deleted_at IS NULL OR @include_deleted::boolean)
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: AdminListFilesCursor :many
SELECT * FROM files
WHERE tenant_id = @tenant_id::uuid
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: ListFilesCursor :many
-- Tenant-wide page used by the collection route for an admin caller.
-- include_deleted is the caller's includeDeleted query (admins also use it).
SELECT * FROM files
WHERE tenant_id = @tenant_id::uuid
  AND (deleted_at IS NULL OR @include_deleted::boolean)
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: TenantStorageUsage :one
-- Tenant usage is the sum of per-user counters, so this is O(users), never
-- O(files), and user-only quota checks never read it.
SELECT COALESCE(SUM(bytes), 0)::bigint AS bytes
FROM files_storage_usage
WHERE tenant_id = @tenant_id::uuid;

-- name: UserStorageUsage :one
SELECT COALESCE((
    SELECT bytes FROM files_storage_usage
    WHERE tenant_id = @tenant_id::uuid AND user_id = @user_id::uuid
), 0)::bigint AS bytes;

-- name: LockTenantQuota :exec
-- Serialize authoritative tenant-quota checks without writing a shared tenant
-- aggregate row. This is taken only when the tenant quota is enabled.
SELECT pg_advisory_xact_lock(
    hashtextextended('files.quota:' || @tenant_id::text, 0)
);

-- name: LockUserStorageUsage :one
INSERT INTO files_storage_usage (tenant_id, user_id, bytes)
VALUES (@tenant_id::uuid, @user_id::uuid, 0)
ON CONFLICT ON CONSTRAINT uq_files_storage_usage_tenant_user DO UPDATE
SET bytes = files_storage_usage.bytes
RETURNING bytes;

-- name: ListFileStoragePaths :many
-- The orphan sweep checks candidate storage keys against the owning tenant's
-- rows; a key with no row is an orphan.
SELECT storage_path FROM files
WHERE tenant_id = @tenant_id::uuid
  AND storage_path = ANY(@paths::text[]);
