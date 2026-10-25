-- Privacy export/erasure for per-user feature-flag targets only. Tenant-wide
-- targets (user_id IS NULL) and flag definitions stay.

-- name: ListFeatureFlagTargetsForPrivacy :many
SELECT * FROM feature_flag_targets
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
ORDER BY id;

-- name: DeleteFeatureFlagTargetsForPrivacy :execrows
DELETE FROM feature_flag_targets
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint;
