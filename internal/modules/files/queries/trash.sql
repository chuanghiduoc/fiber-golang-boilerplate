-- Trash retention (files.trash.purge). These are the only files queries
-- without a tenant_id predicate: the purge is a deliberate cross-tenant
-- maintenance pass, and each row carries its own tenant and storage path.

-- name: LockTrashedFiles :many
-- One bounded batch of rows soft-deleted more than @retention_days ago, oldest
-- first, locked for the purge transaction. SKIP LOCKED lets a concurrent purge
-- take the next rows instead of waiting. Served by idx_files_deleted_at.
SELECT id, tenant_id, storage_path FROM files
WHERE deleted_at IS NOT NULL
  AND deleted_at < now() - make_interval(days => @retention_days::int)
ORDER BY deleted_at
LIMIT @batch_size::int
FOR UPDATE SKIP LOCKED;

-- name: HardDeleteTrashedFiles :execrows
-- Rows are still locked by LockTrashedFiles in the same transaction, so a
-- restore cannot slip in between; the deleted_at guard is defense in depth.
DELETE FROM files
WHERE id = ANY(@ids::bigint[])
  AND deleted_at IS NOT NULL;
