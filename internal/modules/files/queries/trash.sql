-- Trash retention (files.trash.purge). These are the only files queries
-- without a tenant_id predicate: the purge is a deliberate cross-tenant
-- maintenance pass, and each row carries its own tenant and storage path.

-- name: ClaimTrashedFiles :many
-- Committed before any blob delete; RestoreFile skips claimed rows.
-- Served by idx_files_deleted_at.
UPDATE files SET purge_started_at = COALESCE(purge_started_at, now())
WHERE id IN (
    SELECT id FROM files
    WHERE deleted_at IS NOT NULL
      AND deleted_at < now() - make_interval(days => @retention_days::int)
    ORDER BY deleted_at
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
RETURNING id, tenant_id, storage_path;

-- name: HardDeleteTrashedFiles :execrows
DELETE FROM files
WHERE id = ANY(@ids::bigint[])
  AND deleted_at IS NOT NULL
  AND purge_started_at IS NOT NULL;
