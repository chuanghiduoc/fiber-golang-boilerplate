-- ---------------------------------------------------------------------------
-- features: feature flags (the evaluator lives in internal/platform/features)
--
-- feature_flags is process-wide configuration (compile-time keys, database
-- defaults, and emergency kill switches). feature_flag_targets carries the
-- tenant/user overrides and is therefore tenant-owned: tenant_id comes from
-- the request context and the composite FK prevents a user target from
-- pointing at a user in another tenant. The evaluator precedence is
-- force-off > kill switch > user target > tenant target > database default >
-- environment > compiled default.
--
-- feature_flags.enabled is nullable: NULL means "no database default", so a
-- row that only anchors targets (or carries a kill switch) does not override
-- FEATURE_FLAGS/compiled defaults for every tenant. Targets reference the flag
-- with ON DELETE RESTRICT so deleting a process-wide flag can never silently
-- wipe other tenants' targets.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS feature_flags (
    flag_key VARCHAR(128) PRIMARY KEY,
    enabled BOOLEAN,
    kill_switch BOOLEAN NOT NULL DEFAULT FALSE,
    description VARCHAR(500) NOT NULL DEFAULT '',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

DROP TRIGGER IF EXISTS trigger_feature_flags_updated_at ON feature_flags;
CREATE TRIGGER trigger_feature_flags_updated_at
    BEFORE UPDATE ON feature_flags
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS feature_flag_targets (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    flag_key VARCHAR(128) NOT NULL,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT,
    enabled BOOLEAN NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT feature_flag_targets_flag_key_fk
        FOREIGN KEY (flag_key) REFERENCES feature_flags (flag_key) ON DELETE RESTRICT,
    CONSTRAINT feature_flag_targets_user_tenant_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

-- One tenant-wide target (user_id IS NULL) and one target per user per flag:
-- NULLS NOT DISTINCT makes the NULL tenant-wide target collide with itself,
-- so a single constraint (and a single ON CONFLICT target) covers both kinds.
-- It is also the tenant-leading index of the admin list query.
CREATE UNIQUE INDEX IF NOT EXISTS idx_feature_flag_targets_tenant_flag_user
    ON feature_flag_targets (tenant_id, flag_key, user_id) NULLS NOT DISTINCT;
-- Backs the feature_flag_targets_flag_key_fk RESTRICT check that runs on
-- every DeleteFeatureFlag; the unique index above leads with tenant_id.
CREATE INDEX IF NOT EXISTS idx_feature_flag_targets_flag_key
    ON feature_flag_targets (flag_key);

DROP TRIGGER IF EXISTS trigger_feature_flag_targets_updated_at ON feature_flag_targets;
CREATE TRIGGER trigger_feature_flag_targets_updated_at
    BEFORE UPDATE ON feature_flag_targets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();
