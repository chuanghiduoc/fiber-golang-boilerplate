-- name: CreateFile :one
INSERT INTO files (user_id, original_name, storage_path, mime_type, size)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetFileByID :one
SELECT * FROM files WHERE id = $1 AND deleted_at IS NULL;

-- name: ListFilesByUserID :many
SELECT * FROM files WHERE user_id = $1 AND deleted_at IS NULL ORDER BY id DESC LIMIT $2 OFFSET $3;

-- name: CountFilesByUserID :one
SELECT count(*) FROM files WHERE user_id = $1 AND deleted_at IS NULL;

-- name: DeleteFile :one
UPDATE files SET deleted_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: RestoreFile :one
UPDATE files SET deleted_at = NULL
WHERE id = $1 AND deleted_at IS NOT NULL
RETURNING *;

-- name: AdminListFiles :many
SELECT * FROM files ORDER BY id DESC LIMIT $1 OFFSET $2;

-- name: AdminCountFiles :one
SELECT count(*) FROM files;

-- name: ListFilesByUserCursor :many
SELECT * FROM files
WHERE user_id = @user_id::bigint AND deleted_at IS NULL
  AND (NOT @has_cursor::boolean OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: AdminListFilesCursor :many
SELECT * FROM files
WHERE (NOT @has_cursor::boolean OR (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;
