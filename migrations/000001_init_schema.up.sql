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
--   * every tenant-owned row carries tenant_id and tenant-leading indexes;
--   * surrogate keys are BIGINT GENERATED ALWAYS AS IDENTITY, so an explicit
--     id needs OVERRIDING SYSTEM VALUE (only the default tenant seed uses it);
--   * a cross-module child of users references (tenant_id, user_id) with
--     ON DELETE RESTRICT: users are soft-deleted and another module's rows
--     must never vanish through a cascade it does not own.

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

-- ---------------------------------------------------------------------------
-- apikeys: tenant/user-bound API credentials (only SHA-256 hashes at rest)
--
-- prefix_<32-byte base64url> credentials are shown once at creation/rotation.
-- A rotated key remains valid until rotation_grace_until; explicit revocation
-- always wins. Scopes are an authz.Permission array validated by the module.
-- The stored permission strings (e.g. "files:read") are a persisted, stable
-- API: renaming a permission in authz requires a data migration of scopes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    -- The composite FK below makes the database reject a key whose owner
    -- belongs to a different tenant (parent key: idx_users_tenant_id_id).
    user_id BIGINT NOT NULL,
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
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_api_keys_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE RESTRICT
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
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    original_name VARCHAR(255) NOT NULL,
    storage_path VARCHAR(512) NOT NULL UNIQUE,
    mime_type VARCHAR(127) NOT NULL,
    size BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    CONSTRAINT fk_files_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE RESTRICT
);

-- Deliberately NOT partial: it serves ListFilesByUserCursor for both
-- include_deleted values under a generic plan, ListFilesByUserID and
-- CountFilesByUserID (deleted_at is a cheap heap filter), and the
-- fk_files_tenant_user check when a users row is deleted or re-keyed.
CREATE INDEX IF NOT EXISTS idx_files_tenant_user_created
    ON files (tenant_id, user_id, created_at DESC, id DESC);
-- AdminListFilesCursor / AdminListFiles / AdminFileStats.
CREATE INDEX IF NOT EXISTS idx_files_tenant_created_at_id
    ON files (tenant_id, created_at DESC, id DESC);

DROP TRIGGER IF EXISTS trigger_files_updated_at ON files;
CREATE TRIGGER trigger_files_updated_at
    BEFORE UPDATE ON files
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

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
        FOREIGN KEY (tenant_id, user_id) REFERENCES users (tenant_id, id) ON DELETE RESTRICT
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

-- notifications: per-user in-app feed and channel preferences
--
-- A notification row is the in-app delivery. Email delivery is a durable job
-- enqueued in the same transaction as the row, so a queue failure cannot leave
-- a half-delivered notification behind. Users may disable either channel per
-- type; a missing preference row means both channels are enabled.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS notifications (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    type VARCHAR(100) NOT NULL,
    title VARCHAR(255) NOT NULL,
    body TEXT NOT NULL,
    data JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_notifications_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE RESTRICT
);

-- The non-partial index also serves the fk_notifications_tenant_user check.
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_user_created_at_id
    ON notifications (tenant_id, user_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_user_unread
    ON notifications (tenant_id, user_id, created_at DESC, id DESC)
    WHERE read_at IS NULL;

DROP TRIGGER IF EXISTS trigger_notifications_updated_at ON notifications;
CREATE TRIGGER trigger_notifications_updated_at
    BEFORE UPDATE ON notifications
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS notification_preferences (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    type VARCHAR(100) NOT NULL,
    channel VARCHAR(20) NOT NULL CHECK (channel IN ('in_app', 'email')),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_notification_preferences_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES users (tenant_id, id) ON DELETE RESTRICT
);

-- Also serves the fk_notification_preferences_tenant_user check.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_preferences_unique
    ON notification_preferences (tenant_id, user_id, type, channel);

DROP TRIGGER IF EXISTS trigger_notification_preferences_updated_at ON notification_preferences;
CREATE TRIGGER trigger_notification_preferences_updated_at
    BEFORE UPDATE ON notification_preferences
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- platform: durable jobs / outbox
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS jobs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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
    -- lease_expires_at is the owning worker's lease deadline, written as
    -- now() + that worker's lease on claim and on every heartbeat. Keeping it
    -- per row means a worker configured with a short lease cannot reclaim a
    -- healthy job held by a worker with a longer lease.
    lease_expires_at TIMESTAMP WITH TIME ZONE,
    last_error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- A running row without a deadline could never be reclaimed after its
    -- worker crashed, so the database refuses it.
    CONSTRAINT jobs_running_has_lease
        CHECK (status <> 'running' OR lease_expires_at IS NOT NULL)
);

-- claimJobsSQL is a cross-tenant pass: the due CTE (status = 'queued',
-- run_at <= now()) uses this. Claim and purge are global by design, so there
-- is no tenant-leading status/kind index; the only tenant-scoped lookup is the
-- idempotency key.
CREATE INDEX IF NOT EXISTS idx_jobs_status_run_at ON jobs (status, run_at);
-- The lease-expiry CTE scans running rows by deadline
-- (status = 'running' AND lease_expires_at < now()); the partial predicate
-- keeps the index as small as the in-flight set.
CREATE INDEX IF NOT EXISTS idx_jobs_running_lease_expires_at
    ON jobs (lease_expires_at)
    WHERE status = 'running';
-- Keys are scoped by tenant because callers may use user-provided keys.
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_tenant_idempotency_key
    ON jobs (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- Supports the batched retention purge, which orders terminal rows by age.
-- The predicate matches purgeTerminalJobsSQL so the planner can use it.
CREATE INDEX IF NOT EXISTS idx_jobs_terminal_updated_at
    ON jobs (updated_at)
    WHERE status IN ('succeeded', 'dead');

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
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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

-- The application only inserts rows and completes them by primary key; the
-- secondary indexes back operator investigation queries, always tenant-scoped
-- (by time, actor, action, or resource), plus request-id correlation, parent
-- (intent -> domain event) lookups, and the stale-intent check in
-- docs/operations.md (outcome = 'started' AND created_at < ...). The global
-- actor/action/resource duplicates of the tenant-leading indexes were dropped:
-- the table is append-only and write-heavy, and investigations are per tenant.
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

-- Deleting audit rows is never an application path. A reviewed retention or
-- erasure job must disable these triggers explicitly as the table owner.
CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events rows are append-only (% is not allowed)', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_audit_events_no_delete ON audit_events;
CREATE TRIGGER trigger_audit_events_no_delete
    BEFORE DELETE ON audit_events
    FOR EACH ROW
    EXECUTE FUNCTION audit_events_append_only();

DROP TRIGGER IF EXISTS trigger_audit_events_no_truncate ON audit_events;
CREATE TRIGGER trigger_audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT
    EXECUTE FUNCTION audit_events_append_only();

-- ---------------------------------------------------------------------------
-- webhooks: outbound endpoints and versioned signing secrets
--
-- Secrets are sealed with AES-256-GCM by the webhooks module and only the
-- ciphertext is stored. Rotating a secret deactivates the old row and inserts a
-- new version; queued deliveries keep the ciphertext snapshot they were
-- created with, so rotation never breaks in-flight work.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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

-- Parent key for the tenant-composite FKs from secrets and deliveries.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant_id_id
    ON webhook_endpoints (tenant_id, id);
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
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    endpoint_id BIGINT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    secret_ciphertext TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- Secrets are fully owned by their endpoint (same module), so CASCADE.
    CONSTRAINT fk_webhook_endpoint_secrets_tenant_endpoint FOREIGN KEY (tenant_id, endpoint_id)
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE CASCADE
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
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    -- Deliveries are history: endpoints are soft-deleted and a hard delete
    -- must not silently erase the delivery log (RESTRICT, tenant-composite).
    endpoint_id BIGINT NOT NULL,
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
    -- Per-request timeout snapshotted from the endpoint, like url/secret.
    timeout_ms INTEGER NOT NULL DEFAULT 10000
        CHECK (timeout_ms BETWEEN 100 AND 120000),
    next_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_status_code INTEGER,
    last_error TEXT,
    delivered_at TIMESTAMP WITH TIME ZONE,
    -- Single-column on purpose: a composite FK with ON DELETE SET NULL would
    -- also null tenant_id when the original delivery is purged.
    replayed_from_id BIGINT REFERENCES webhook_deliveries(id) ON DELETE SET NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_webhook_deliveries_tenant_endpoint FOREIGN KEY (tenant_id, endpoint_id)
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_endpoint_event
    ON webhook_deliveries (tenant_id, endpoint_id, event_id);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_created_at_id
    ON webhook_deliveries (tenant_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_endpoint_created
    ON webhook_deliveries (tenant_id, endpoint_id, created_at DESC, id DESC);
-- Retention is a cross-tenant maintenance pass, so these purge indexes are
-- deliberately not tenant-leading. The predicate matches PurgeWebhookDeliveries.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_terminal_updated_at
    ON webhook_deliveries (updated_at)
    WHERE status IN ('succeeded', 'dead');
-- Purging an original delivery runs ON DELETE SET NULL against its replays;
-- without this index every deleted row would scan webhook_deliveries.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_replayed_from_id
    ON webhook_deliveries (replayed_from_id)
    WHERE replayed_from_id IS NOT NULL;

DROP TRIGGER IF EXISTS trigger_webhook_deliveries_updated_at ON webhook_deliveries;
CREATE TRIGGER trigger_webhook_deliveries_updated_at
    BEFORE UPDATE ON webhook_deliveries
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS webhook_delivery_attempts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    -- CASCADE by design: attempts are fully owned by their delivery, and the
    -- batched retention purge (PurgeWebhookDeliveries) deletes deliveries and
    -- relies on this cascade to remove their attempts in the same statement.
    -- Kept single-column: a tenant-composite FK would need another unique
    -- (tenant_id, id) index on the high-volume webhook_deliveries table, and
    -- attempts are only written by the worker with the delivery's tenant_id.
    delivery_id BIGINT NOT NULL REFERENCES webhook_deliveries(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    status_code INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_delivery_attempts_delivery_attempt
    ON webhook_delivery_attempts (delivery_id, attempt);
-- Tenant-leading index for ListWebhookDeliveryAttempts. Attempts have no
-- time-ordered listing, so there is no (tenant_id, created_at) index.
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_attempts_tenant_delivery
    ON webhook_delivery_attempts (tenant_id, delivery_id);

-- ---------------------------------------------------------------------------
-- webhooks: inbound sources and deduplicated inbound events
--
-- The public receiver authenticates HMAC over the raw body and timestamp. The
-- source secret is sealed at rest but must be decryptable for verification.
-- Inbound events deduplicate on (tenant, source, external_event_id).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_inbound_sources (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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
-- Parent key for the tenant-composite FK from webhook_inbound_events.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_inbound_sources_tenant_id_id
    ON webhook_inbound_sources (tenant_id, id);
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_sources_tenant_created
    ON webhook_inbound_sources (tenant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trigger_webhook_inbound_sources_updated_at ON webhook_inbound_sources;
CREATE TRIGGER trigger_webhook_inbound_sources_updated_at
    BEFORE UPDATE ON webhook_inbound_sources
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE IF NOT EXISTS webhook_inbound_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    source_id BIGINT NOT NULL,
    external_event_id VARCHAR(255) NOT NULL,
    event_type VARCHAR(128) NOT NULL DEFAULT '',
    signature VARCHAR(255) NOT NULL,
    payload JSONB NOT NULL,
    received_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- Inbound events are history and sources are soft-deleted: RESTRICT so a
    -- hard delete of a source cannot erase its events; retention purges them.
    CONSTRAINT fk_webhook_inbound_events_tenant_source FOREIGN KEY (tenant_id, source_id)
        REFERENCES webhook_inbound_sources (tenant_id, id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_inbound_events_source_event
    ON webhook_inbound_events (tenant_id, source_id, external_event_id);
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_events_tenant_source_received
    ON webhook_inbound_events (tenant_id, source_id, received_at DESC, id DESC);
-- Cross-tenant retention purge (PurgeWebhookInboundEvents).
CREATE INDEX IF NOT EXISTS idx_webhook_inbound_events_received_at
    ON webhook_inbound_events (received_at);
