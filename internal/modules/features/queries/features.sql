-- Feature flag state is owned by internal/platform/features; this module owns
-- the admin transport and its persistence queries. User targets always carry
-- the request tenant, enforced by the (tenant_id, user_id) foreign key.

-- name: ListFeatureFlags :many
SELECT * FROM feature_flags
ORDER BY flag_key;

-- name: GetFeatureFlag :one
SELECT * FROM feature_flags
WHERE flag_key = $1;

-- name: UpsertFeatureFlag :one
INSERT INTO feature_flags (flag_key, enabled, kill_switch, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (flag_key) DO UPDATE
SET enabled = EXCLUDED.enabled,
    kill_switch = EXCLUDED.kill_switch,
    description = EXCLUDED.description,
    updated_at = NOW()
RETURNING *;

-- name: DeleteFeatureFlag :one
DELETE FROM feature_flags
WHERE flag_key = $1
RETURNING flag_key;

-- name: ListFeatureFlagTargets :many
SELECT * FROM feature_flag_targets
WHERE tenant_id = $1 AND flag_key = $2
ORDER BY user_id NULLS FIRST, id;

-- name: UpsertFeatureFlagTenantTarget :one
INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled)
VALUES ($1, $2, NULL, $3)
ON CONFLICT (flag_key, tenant_id) WHERE user_id IS NULL
DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()
RETURNING *;

-- name: UpsertFeatureFlagUserTarget :one
INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (flag_key, tenant_id, user_id) WHERE user_id IS NOT NULL
DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()
RETURNING *;

-- name: DeleteFeatureFlagTarget :one
DELETE FROM feature_flag_targets
WHERE id = $1 AND tenant_id = $2 AND flag_key = $3
RETURNING id;
