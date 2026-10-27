-- Tenant purge. The caller deletes each batch's blobs first and then the rows;
-- a retry re-reads the rows, treats a missing blob as deleted, and finishes.

-- name: ListTenantFilesForPurge :many
SELECT id, storage_path FROM files
WHERE tenant_id = @tenant_id::uuid
LIMIT @batch_size::int;

-- name: DeleteTenantFilesForPurge :execrows
DELETE FROM files
WHERE tenant_id = @tenant_id::uuid
  AND id = ANY(@ids::uuid[]);
