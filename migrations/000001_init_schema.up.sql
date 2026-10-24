-- Consolidated initial schema for the modular monolith.
-- This is the only schema file. Add additive changes as new numbered
-- migrations only after the project has a deployed database; for the
-- boilerplate, fresh installs run this file exactly once via cmd/migrate.
--
-- Conventions:
--   * one module owns each table (prefix/name documents the owner);
--   * active-row uniqueness uses partial indexes (soft delete friendly);
--   * cursor pagination is backed by (created_at DESC, id DESC) indexes;
--   * timestamps are NOT NULL so pagination cannot hide rows;
--   * every tenant-owned row carries tenant_id and tenant-leading indexes.

CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- platform: tenants
--
-- The default tenant is inserted as the first row of a fresh database, so
-- BIGSERIAL assigns id 1. Rows that predate multi-tenancy default to that id.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenants (
    id BIGSERIAL PRIMARY KEY,
    slug VARCHAR(63) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'suspended')),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

INSERT INTO tenants (slug, name)
VALUES ('default', 'Default')
ON CONFLICT (slug) DO NOTHING;

DROP TRIGGER IF EXISTS trigger_tenants_updated_at ON tenants;
CREATE TRIGGER trigger_tenants_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: users
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
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

DROP TRIGGER IF EXISTS trigger_users_updated_at ON users;
CREATE TRIGGER trigger_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- At least one active administrator must always remain in each tenant, even
-- under concurrent demote/ban requests. Application checks return a friendly
-- 409; this trigger is the database-level guarantee. Counting and locking are
-- scoped to OLD.tenant_id so one tenant's admins cannot satisfy another's.
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
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token VARCHAR(255) NOT NULL UNIQUE,
    token_version INTEGER NOT NULL DEFAULT 0,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_token ON refresh_tokens(token);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_id ON refresh_tokens(user_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_tenant_user_id
    ON refresh_tokens (tenant_id, user_id);

-- ---------------------------------------------------------------------------
-- identity: password-reset tokens (one live token per user)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_password_reset_tokens_user_id
    ON password_reset_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_tenant_user_id
    ON password_reset_tokens (tenant_id, user_id);

-- ---------------------------------------------------------------------------
-- identity: email-verification tokens (bound to the target address)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_verification_tokens (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    token VARCHAR(255) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_email_verification_tokens_user_id
    ON email_verification_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_tenant_user_id
    ON email_verification_tokens (tenant_id, user_id);

-- ---------------------------------------------------------------------------
-- files: metadata (blobs live in local/S3 storage)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS files (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_name VARCHAR(255) NOT NULL,
    storage_path VARCHAR(512) NOT NULL UNIQUE,
    mime_type VARCHAR(127) NOT NULL,
    size BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_files_user_id ON files(user_id);
CREATE INDEX IF NOT EXISTS idx_files_deleted_at ON files(deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_files_user_id_deleted_at ON files(user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_files_user_created ON files(user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_files_created_at_id ON files(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_files_tenant_user_id_deleted_at
    ON files (tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_files_tenant_user_created
    ON files (tenant_id, user_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_files_tenant_created_at_id
    ON files (tenant_id, created_at DESC, id DESC);

-- ---------------------------------------------------------------------------
-- platform: durable jobs / outbox
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS jobs (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    kind TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- W3C traceparent captured when the job was enqueued. The worker uses it
    -- for a span link only; it never affects tenant scoping or retries.
    trace_parent TEXT,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'dead')),
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    run_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    locked_at TIMESTAMP WITH TIME ZONE,
    locked_by TEXT,
    last_error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_jobs_status_run_at ON jobs (status, run_at);
CREATE INDEX IF NOT EXISTS idx_jobs_kind_status ON jobs (kind, status);
CREATE INDEX IF NOT EXISTS idx_jobs_tenant_status_run_at
    ON jobs (tenant_id, status, run_at);
CREATE INDEX IF NOT EXISTS idx_jobs_tenant_kind_status
    ON jobs (tenant_id, kind, status);

-- ---------------------------------------------------------------------------
-- platform: audit trail
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_events (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    actor_id BIGINT,
    action TEXT NOT NULL,
    resource TEXT NOT NULL,
    resource_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip TEXT,
    user_agent TEXT,
    request_id TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_actor_created
    ON audit_events (actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_action_created
    ON audit_events (action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_resource
    ON audit_events (resource, resource_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_created
    ON audit_events (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_actor_created
    ON audit_events (tenant_id, actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_action_created
    ON audit_events (tenant_id, action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_resource
    ON audit_events (tenant_id, resource, resource_id);
