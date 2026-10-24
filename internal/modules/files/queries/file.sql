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
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND (deleted_at IS NULL OR @include_deleted::boolean)
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: AdminListFilesCursor :many
SELECT * FROM files
WHERE tenant_id = @tenant_id::bigint
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;
