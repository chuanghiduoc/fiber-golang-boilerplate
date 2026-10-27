-- Tenant purge. Removes tenant- and user-scoped targets in bounded id batches;
-- the process-wide feature_flags table is deliberately untouched.

-- name: DeleteTenantFeatureFlagTargetsBatch :execrows
DELETE FROM feature_flag_targets
WHERE id IN (
    SELECT id FROM feature_flag_targets
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);
