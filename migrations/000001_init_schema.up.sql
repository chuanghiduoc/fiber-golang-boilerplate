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
-- apikeys: tenant/user-bound API credentials (only SHA-256 hashes at rest)
--
-- prefix_<32-byte base64url> credentials are shown once at creation/rotation.
-- A rotated key remains valid until rotation_grace_until; explicit revocation
-- always wins. Scopes are an authz.Permission array validated by the module.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    prefix VARCHAR(16) NOT NULL,
    key_hash VARCHAR(64) NOT NULL,
    scopes JSONB NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(scopes) = 'array'),
    expires_at TIMESTAMP WITH TIME ZONE,
    revoked_at TIMESTAMP WITH TIME ZONE,
    rotated_at TIMESTAMP WITH TIME ZONE,
    rotation_grace_until TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
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
-- platform: feature flags
--
-- feature_flags is process-wide configuration (compile-time keys, database
-- defaults, and emergency kill switches). feature_flag_targets carries the
-- tenant/user overrides and is therefore tenant-owned: tenant_id comes from
-- the request context and the composite FK prevents a user target from
-- pointing at a user in another tenant. The evaluator precedence is
-- force-off > kill switch > user target > tenant target > database default >
-- environment > compiled default.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS feature_flags (
    flag_key VARCHAR(128) PRIMARY KEY,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
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
    id BIGSERIAL PRIMARY KEY,
    flag_key VARCHAR(128) NOT NULL REFERENCES feature_flags(flag_key) ON DELETE CASCADE,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT,
    enabled BOOLEAN NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT feature_flag_targets_user_tenant_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id) ON DELETE CASCADE
);

-- One tenant-wide target (user_id IS NULL) and one target per user are
-- allowed per flag. The tenant-leading index backs the admin list query.
CREATE UNIQUE INDEX IF NOT EXISTS idx_feature_flag_targets_tenant_target
    ON feature_flag_targets (flag_key, tenant_id) WHERE user_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_feature_flag_targets_user_target
    ON feature_flag_targets (flag_key, tenant_id, user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_feature_flag_targets_tenant_flag_user
    ON feature_flag_targets (tenant_id, flag_key, user_id);

DROP TRIGGER IF EXISTS trigger_feature_flag_targets_updated_at ON feature_flag_targets;
CREATE TRIGGER trigger_feature_flag_targets_updated_at
    BEFORE UPDATE ON feature_flag_targets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

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
    -- idempotency_key deduplicates producer retries (event fan-out, scheduled
    -- slots). NULL means the job is not deduplicated.
    idempotency_key TEXT,
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
-- Keys are scoped by tenant because callers may use user-provided keys.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_tenant_idempotency_key
    ON jobs (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- platform: scheduler leader lease and run ledger
--
-- The lease is a single row per scheduler group; the holder renews it until it
-- exits or loses the lease. scheduler_runs is the unique ledger that prevents
-- duplicate enqueues for the same schedule slot, even across leaders.
-- These tables deliberately have no foreign keys: the scheduler is a platform
-- component and its rows must survive tenant/module lifecycle changes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS scheduler_leases (
    name TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    heartbeat_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS scheduler_runs (
    id BIGSERIAL PRIMARY KEY,
    schedule TEXT NOT NULL,
    scheduled_for TIMESTAMP WITH TIME ZONE NOT NULL,
    status TEXT NOT NULL DEFAULT 'enqueued'
        CHECK (status IN ('enqueued', 'skipped', 'failed')),
    job_id BIGINT,
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_runs_schedule_slot
    ON scheduler_runs (schedule, scheduled_for);
CREATE INDEX IF NOT EXISTS idx_scheduler_runs_created_at
    ON scheduler_runs (created_at);

-- ---------------------------------------------------------------------------
-- platform: audit trail
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_events (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    parent_id BIGINT,
    actor_id BIGINT,
    actor_type TEXT NOT NULL DEFAULT 'unknown'
        CHECK (actor_type IN ('user', 'admin', 'service', 'job', 'system', 'anonymous', 'unknown')),
    actor_role TEXT,
    action TEXT NOT NULL,
    resource TEXT NOT NULL,
    resource_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip TEXT,
    user_agent TEXT,
    request_id TEXT,
    source TEXT NOT NULL DEFAULT 'http'
        CHECK (source IN ('http', 'service', 'job', 'system')),
    outcome TEXT NOT NULL DEFAULT 'succeeded'
        CHECK (outcome IN ('started', 'succeeded', 'failed', 'denied', 'unknown')),
    status_code SMALLINT,
    error_code TEXT,
    idempotency_key_hash TEXT,
    completed_at TIMESTAMP WITH TIME ZONE,
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
CREATE INDEX IF NOT EXISTS idx_audit_events_request_id
    ON audit_events (request_id)
    WHERE request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_events_parent_id
    ON audit_events (parent_id)
    WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_events_started
    ON audit_events (tenant_id, created_at)
    WHERE outcome = 'started';

-- Audit rows are append-only. Only a pre-write intent row (source http,
-- outcome started) may transition to a terminal outcome, and only completion
-- fields may change. This makes a successful mutation's audit row immutable.
CREATE OR REPLACE FUNCTION audit_events_completion_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.source <> 'http' OR OLD.outcome <> 'started' THEN
        RAISE EXCEPTION 'audit_events rows are immutable after completion';
    END IF;
    IF NEW.source <> 'http' THEN
        RAISE EXCEPTION 'audit completion cannot change source';
    END IF;
    IF NEW.outcome NOT IN ('succeeded', 'failed', 'denied', 'unknown') THEN
        RAISE EXCEPTION 'audit completion requires a terminal outcome';
    END IF;
    IF NEW.completed_at IS NULL THEN
        RAISE EXCEPTION 'audit completion requires completed_at';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.parent_id IS DISTINCT FROM OLD.parent_id
        OR NEW.action IS DISTINCT FROM OLD.action
        OR NEW.resource IS DISTINCT FROM OLD.resource
        OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
        OR NEW.metadata IS DISTINCT FROM OLD.metadata
        OR NEW.ip IS DISTINCT FROM OLD.ip
        OR NEW.user_agent IS DISTINCT FROM OLD.user_agent
        OR NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.idempotency_key_hash IS DISTINCT FROM OLD.idempotency_key_hash
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'audit completion may only change completion fields';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_audit_events_completion ON audit_events;
CREATE TRIGGER trigger_audit_events_completion
    BEFORE UPDATE ON audit_events
    FOR EACH ROW
    EXECUTE FUNCTION audit_events_completion_guard();

-- ---------------------------------------------------------------------------
-- webhooks: outbound endpoints and versioned signing secrets
--
-- Secrets are sealed with AES-256-GCM by the webhooks module and only the
-- ciphertext is stored. Rotating a secret deactivates the old row and inserts a
-- new version; queued deliveries keep the ciphertext snapshot they were
-- created with, so rotation never breaks in-flight work.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    url VARCHAR(2048) NOT NULL,
    description VARCHAR(255) NOT NULL DEFAULT '',
    event_types TEXT[] NOT NULL DEFAULT ARRAY['*']::TEXT[],
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    max_attempts INTEGER NOT NULL DEFAULT 8
        CHECK (max_attempts BETWEEN 1 AND 50),
    timeout_ms INTEGER NOT NULL DEFAULT 10000
        CHECK (timeout_ms BETWEEN 100 AND 120000),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant_created_at_id
    ON webhook_endpoints (tenant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant_status
    ON webhook_endpoints (tenant_id, status)
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trigger_webhook_endpoints_updated_at ON webhook_endpoints;
CREATE TRIGGER trigger_webhook_endpoints_updated_at
    BEFORE UPDATE ON webhook_endpoints
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS webhook_endpoint_secrets (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    endpoint_id BIGINT NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    secret_ciphertext TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_endpoint_secrets_endpoint_version
    ON webhook_endpoint_secrets (endpoint_id, version);
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_endpoint_secrets_active
    ON webhook_endpoint_secrets (tenant_id, endpoint_id)
    WHERE active;
CREATE INDEX IF NOT EXISTS idx_webhook_endpoint_secrets_tenant_endpoint
    ON webhook_endpoint_secrets (tenant_id, endpoint_id);

-- ---------------------------------------------------------------------------
-- webhooks: delivery outbox, delivery attempts, retained payload snapshots
--
-- A delivery row and its first durable job commit atomically. event_id is
-- deduplicated per endpoint so re-dispatching the same domain event is safe.
-- payload/url/secret_ciphertext are snapshots: editing or rotating an endpoint
-- cannot change a queued delivery's signed request.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    endpoint_id BIGINT NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_id VARCHAR(128) NOT NULL,
    event_type VARCHAR(128) NOT NULL,
    event_created_at TIMESTAMP WITH TIME ZONE NOT NULL,
    url VARCHAR(2048) NOT NULL,
    payload JSONB NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'succeeded', 'dead')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts BETWEEN 1 AND 50),
    next_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_status_code INTEGER,
    last_error TEXT,
    delivered_at TIMESTAMP WITH TIME ZONE,
    replayed_from_id BIGINT REFERENCES webhook_deliveries(id) ON DELETE SET NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_endpoint_event
    ON webhook_deliveries (tenant_id, endpoint_id, event_id);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_created_at_id
    ON webhook_deliveries (tenant_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_endpoint_created
    ON webhook_deliveries (tenant_id, endpoint_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_status_next
    ON webhook_deliveries (tenant_id, status, next_attempt_at);

DROP TRIGGER IF EXISTS trigger_webhook_deliveries_updated_at ON webhook_deliveries;
CREATE TRIGGER trigger_webhook_deliveries_updated_at
    BEFORE UPDATE ON webhook_deliveries
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS webhook_delivery_attempts (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    delivery_id BIGINT NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    status_code INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_delivery_attempts_delivery_attempt
    ON webhook_delivery_attempts (delivery_id, attempt);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_attempts_tenant_delivery
    ON webhook_delivery_attempts (tenant_id, delivery_id);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_attempts_tenant_created
    ON webhook_delivery_attempts (tenant_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- webhooks: inbound sources and deduplicated inbound events
--
-- The public receiver authenticates HMAC over the raw body and timestamp. The
-- source secret is sealed at rest but must be decryptable for verification.
-- Inbound events deduplicate on (tenant, source, external_event_id).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_inbound_sources (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(63) NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_inbound_sources_tenant_slug_active
    ON webhook_inbound_sources (tenant_id, slug)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_sources_tenant_created
    ON webhook_inbound_sources (tenant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trigger_webhook_inbound_sources_updated_at ON webhook_inbound_sources;
CREATE TRIGGER trigger_webhook_inbound_sources_updated_at
    BEFORE UPDATE ON webhook_inbound_sources
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS webhook_inbound_events (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    source_id BIGINT NOT NULL REFERENCES webhook_inbound_sources(id) ON DELETE CASCADE,
    external_event_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(128) NOT NULL DEFAULT '',
    signature VARCHAR(255) NOT NULL,
    payload JSONB NOT NULL,
    received_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_inbound_events_source_event
    ON webhook_inbound_events (tenant_id, source_id, external_event_id);
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_events_tenant_source_received
    ON webhook_inbound_events (tenant_id, source_id, received_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_events_tenant_received
    ON webhook_inbound_events (tenant_id, received_at DESC, id DESC);
