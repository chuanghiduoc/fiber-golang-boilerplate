-- Tenant purge. A deleted tenant's API keys are erased in bounded id batches;
-- api_key_scopes is a pure child and cascades with its key.

-- name: DeleteTenantAPIKeysBatch :execrows
DELETE FROM api_keys
WHERE id IN (
    SELECT id FROM api_keys
    WHERE tenant_id = @tenant_id::bigint
    LIMIT @batch_size::int
);
