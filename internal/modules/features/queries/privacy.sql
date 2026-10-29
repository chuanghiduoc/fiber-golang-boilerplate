-- Privacy export/erasure for per-user feature-flag targets only. Tenant-wide
-- targets (user_id IS NULL) and flag definitions stay.

-- name: ListFeatureFlagTargetsForPrivacy :many
SELECT * FROM feature_flag_targets
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
ORDER BY id
LIMIT @row_limit::int;

-- name: DeleteFeatureFlagTargetsForPrivacy :execrows
DELETE FROM feature_flag_targets
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid;
