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

-- name: ListFilesByUserID :many
SELECT * FROM files
WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL
ORDER BY id DESC
LIMIT $3 OFFSET $4;

-- name: CountFilesByUserID :one
SELECT count(*) FROM files
WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: DeleteFile :one
UPDATE files SET deleted_at = NOW()
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreFile :one
UPDATE files SET deleted_at = NULL
WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NOT NULL
  AND purge_started_at IS NULL
RETURNING *;

-- name: AdminListFiles :many
SELECT * FROM files
WHERE tenant_id = $1
ORDER BY id DESC
LIMIT $2 OFFSET $3;

-- name: AdminCountFiles :one
SELECT count(*) FROM files
WHERE tenant_id = $1;

-- name: AdminFileStats :one
SELECT
    count(*) FILTER (WHERE deleted_at IS NULL)::bigint AS total_files,
    COALESCE(SUM(size) FILTER (WHERE deleted_at IS NULL), 0)::bigint AS total_file_size
FROM files
WHERE tenant_id = $1;

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

-- name: LockFilesQuota :exec
-- Serializes uploads of one tenant for the rest of the metadata transaction,
-- so a concurrent upload cannot read the same free space and also pass.
SELECT pg_advisory_xact_lock(hashtextextended('files.quota:' || @tenant_id::uuid, 0));

-- name: FilesStorageUsage :one
-- Both quotas count trashed rows: a soft-deleted blob still occupies storage.
SELECT
    COALESCE(SUM(size), 0)::bigint AS tenant_bytes,
    COALESCE(SUM(size) FILTER (WHERE user_id = @user_id::uuid), 0)::bigint AS user_bytes
FROM files
WHERE tenant_id = @tenant_id::uuid;

-- name: ListFileStoragePaths :many
-- The orphan sweep checks candidate storage keys against the owning tenant's
-- rows; a key with no row is an orphan.
SELECT storage_path FROM files
WHERE tenant_id = @tenant_id::uuid
  AND storage_path = ANY(@paths::text[]);
