-- ---------------------------------------------------------------------------
-- apikeys: tenant/user-bound API credentials (only SHA-256 hashes at rest)
--
-- prefix_<32-byte base64url> credentials are shown once at creation/rotation.
-- A rotated key remains valid until rotation_grace_until; explicit revocation
-- always wins. Scopes are rows of api_key_scopes validated by the module
-- against the authz catalog. The stored permission strings (e.g.
-- "files:read") are a persisted, stable API: renaming a permission in authz
-- requires a data migration of api_key_scopes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    -- The composite FK below makes the database reject a key whose owner
    -- is not a member of the key's tenant (parent key: uq_memberships_tenant_user).
    user_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    prefix VARCHAR(16) NOT NULL,
    key_hash VARCHAR(64) NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE,
    revoked_at TIMESTAMP WITH TIME ZONE,
    rotated_at TIMESTAMP WITH TIME ZONE,
    rotation_grace_until TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_api_keys_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    -- MarkAPIKeyRotated is the only writer of either column and always sets
    -- both, so a grace window exists exactly when the key was rotated.
    CONSTRAINT api_keys_rotation_grace_matches_rotated
        CHECK ((rotation_grace_until IS NULL) = (rotated_at IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash
    ON api_keys (key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_user
    ON api_keys (tenant_id, user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant_created_at_id
    ON api_keys (tenant_id, created_at DESC, id DESC);

DROP TRIGGER IF EXISTS trigger_api_keys_updated_at ON api_keys;
CREATE TRIGGER trigger_api_keys_updated_at
    BEFORE UPDATE ON api_keys
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- One row per granted permission. The rows are fully owned by their key
-- (CASCADE) and carry no tenant_id: the key id is global and the parent row
-- holds the tenant. permission_key is an authz permission; the CHECK mirrors
-- authz.permissionPattern. Once the RBAC phase adds a permissions catalog
-- table, add FOREIGN KEY (permission_key) REFERENCES permissions (key) here.
CREATE TABLE IF NOT EXISTS api_key_scopes (
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    permission_key TEXT NOT NULL
        CHECK (permission_key ~ '^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)+$'),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (api_key_id, permission_key)
);
