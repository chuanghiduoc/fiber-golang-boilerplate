-- Trash retention (files.trash.purge). These are the only files queries
-- without a tenant_id predicate: the purge is a deliberate cross-tenant
-- maintenance pass, and each row carries its own tenant and storage path.

-- name: ClaimTrashedFiles :many
-- Committed before any blob delete; RestoreFile skips claimed rows. Rows whose
-- retry time is still in the future are skipped so a permanently failing blob
-- cannot block newer rows.
-- Served by idx_files_deleted_at.
UPDATE files SET purge_started_at = COALESCE(purge_started_at, now())
WHERE id IN (
    SELECT id FROM files
    WHERE deleted_at IS NOT NULL
      AND deleted_at < now() - make_interval(days => @retention_days::int)
      AND (purge_retry_after IS NULL OR purge_retry_after <= now())
    ORDER BY deleted_at
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
RETURNING id, tenant_id, storage_path;

-- name: FailTrashedFiles :execrows
-- A blob delete failed: keep the claim (the row is never restorable after the
-- first delete attempt), count the attempt, and defer the next try with a
-- capped exponential backoff. Only the blob delete is retried.
UPDATE files
SET purge_attempts = purge_attempts + 1,
    purge_retry_after = now() + make_interval(hours => LEAST(24, 1 << LEAST(purge_attempts, 5)))
WHERE id = ANY(@ids::uuid[])
  AND purge_started_at IS NOT NULL;

-- name: HardDeleteTrashedFiles :execrows
DELETE FROM files
WHERE id = ANY(@ids::uuid[])
  AND deleted_at IS NOT NULL
  AND purge_started_at IS NOT NULL;
