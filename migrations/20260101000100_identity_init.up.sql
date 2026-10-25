-- ---------------------------------------------------------------------------
-- identity: tenants
--
-- The default tenant is pinned to id 1 because every tenant_id column defaults
-- to 1. The identity sequence is then advanced past it so the next tenant
-- gets id 2 instead of colliding with the seed row.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenants (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug VARCHAR(63) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'suspended')),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

INSERT INTO tenants (id, slug, name)
OVERRIDING SYSTEM VALUE
VALUES (1, 'default', 'Default')
ON CONFLICT (slug) DO NOTHING;

SELECT setval(pg_get_serial_sequence('tenants', 'id'), (SELECT max(id) FROM tenants));

DROP TRIGGER IF EXISTS trigger_tenants_updated_at ON tenants;
CREATE TRIGGER trigger_tenants_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: users
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    email VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255),
    name VARCHAR(255) NOT NULL,
    role VARCHAR(50) NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    google_id VARCHAR(255),
    auth_provider VARCHAR(50) NOT NULL DEFAULT 'local' CHECK (auth_provider IN ('local', 'google')),
    email_verified_at TIMESTAMP WITH TIME ZONE,
    token_version INTEGER NOT NULL DEFAULT 0,
    password_changed_at TIMESTAMP WITH TIME ZONE,
    banned_at TIMESTAMP WITH TIME ZONE,
    -- Set by a ban and never cleared by unban: API keys created at or before
    -- it stay invalid after the account is restored.
    credentials_revoked_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

-- Active emails are case-insensitive and unique per tenant; they are reusable
-- after soft delete. The old global unique indexes are replaced by these
-- tenant-scoped ones.
DROP INDEX IF EXISTS idx_users_email_lower_active;
DROP INDEX IF EXISTS idx_users_google_id_active;
DROP INDEX IF EXISTS idx_users_role_active;
DROP INDEX IF EXISTS idx_users_created_at_id;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_email_lower_active
    ON users (tenant_id, lower(email))
    WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_google_id_active
    ON users (tenant_id, google_id)
    WHERE deleted_at IS NULL AND google_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_users_tenant_role_active
    ON users (tenant_id, role)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_tenant_created_at_id
    ON users (tenant_id, created_at DESC, id DESC);
-- GetBannedUserByEmail runs on public registration/email changes; the active
-- partial index above excludes banned (soft-deleted) rows.
CREATE INDEX IF NOT EXISTS idx_users_tenant_email_lower_banned
    ON users (tenant_id, lower(email))
    WHERE banned_at IS NOT NULL;
-- Required as the parent key for tenant-aware feature flag user targets:
-- a target's (tenant_id, user_id) must reference a user in the same tenant.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_id_id
    ON users (tenant_id, id);

DROP TRIGGER IF EXISTS trigger_users_updated_at ON users;
CREATE TRIGGER trigger_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- At least one active administrator must always remain in each tenant, even
-- under concurrent demote/ban requests. This trigger is the database-level
-- guarantee; counting and locking are scoped to OLD.tenant_id so one tenant's
-- admins cannot satisfy another's. The other admins are locked in id order.
-- The UPDATE already holds the target row before the trigger runs, so two
-- concurrent demotions could still wait on each other's rows; identity
-- services therefore take a per-tenant advisory lock (LockTenantAdminChanges)
-- before touching an admin row and return a friendly 409. A deadlock (40P01)
-- or this trigger's exception (P0001) from any other writer is mapped to 409
-- by the identity repository.
CREATE OR REPLACE FUNCTION protect_last_admin()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.role = 'admin'
       AND OLD.deleted_at IS NULL
       AND (TG_OP = 'DELETE' OR NEW.role <> 'admin' OR NEW.deleted_at IS NOT NULL
            OR NEW.tenant_id <> OLD.tenant_id) THEN
        PERFORM 1
        FROM users
        WHERE tenant_id = OLD.tenant_id
          AND role = 'admin'
          AND deleted_at IS NULL
          AND id <> OLD.id
        ORDER BY id
        FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'cannot remove the last active admin';
        END IF;
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_protect_last_admin ON users;
CREATE TRIGGER trigger_protect_last_admin
    BEFORE UPDATE OF role, deleted_at, tenant_id OR DELETE ON users
    FOR EACH ROW
    EXECUTE FUNCTION protect_last_admin();

-- ---------------------------------------------------------------------------
-- identity: refresh tokens (stores SHA-256 hashes, never plaintext)
-- ---------------------------------------------------------------------------
-- Identity token tables are fully owned by their user, so the composite
-- (tenant_id, user_id) FK cascades: the database rejects a token whose tenant
-- differs from its user's (parent key: idx_users_tenant_id_id).
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    token VARCHAR(255) NOT NULL UNIQUE,
    token_version INTEGER NOT NULL DEFAULT 0,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_refresh_tokens_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE CASCADE
);

-- Token lookups use the implicit refresh_tokens_token_key UNIQUE index; every
-- per-user query (and the FK cascade) filters (tenant_id, user_id).
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_tenant_user_id
    ON refresh_tokens (tenant_id, user_id);
-- The scheduled cross-tenant purge (PurgeExpiredRefreshTokens) deletes expired
-- rows in bounded batches. Refresh tokens accrue one row per sign-in, so the
-- purge must not scan the table; the one-row-per-user reset and verification
-- token tables stay small enough to purge without an index.
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at
    ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- identity: password-reset tokens (one live token per user)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_password_reset_tokens_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE CASCADE
);

-- One live token per user (user ids are global, so this is the same rule as a
-- user_id-only key) and the tenant-leading index for every per-user query.
CREATE UNIQUE INDEX IF NOT EXISTS idx_password_reset_tokens_tenant_user_id
    ON password_reset_tokens (tenant_id, user_id);

-- ---------------------------------------------------------------------------
-- identity: email-verification tokens (bound to the target address)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_verification_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    email VARCHAR(255) NOT NULL,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_email_verification_tokens_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE CASCADE
);

-- One live token per user, tenant-leading for every per-user query.
CREATE UNIQUE INDEX IF NOT EXISTS idx_email_verification_tokens_tenant_user_id
    ON email_verification_tokens (tenant_id, user_id);
