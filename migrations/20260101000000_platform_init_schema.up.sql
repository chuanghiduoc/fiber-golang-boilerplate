-- Fresh-install schema (ADR 0012): the single init migration. Each section
-- starts with an `-- owner: <module|platform>` marker, and test/arch checks
-- that a section only creates its owner's tables. Sections run in foreign-key
-- dependency order: shared functions, identity, platform tables, modules.
-- Every later change is a new <UTC timestamp>_<owner>_<purpose> migration;
-- this file is edited in place only until the first release.
--
-- Conventions:
--   * one owner per table; the section marker names the owner;
--   * active-row uniqueness uses partial indexes (soft delete friendly);
--   * cursor pagination is backed by (created_at DESC, id DESC) indexes;
--   * timestamps are NOT NULL so pagination cannot hide rows;
--   * every tenant-owned row carries tenant_id and tenant-leading indexes;
--   * surrogate keys are BIGINT GENERATED ALWAYS AS IDENTITY, so an explicit
--     id needs OVERRIDING SYSTEM VALUE (only the default tenant seed uses it);
--   * a cross-module per-user row references memberships (tenant_id, user_id)
--     with ON DELETE RESTRICT: a module row for a user in a tenant requires
--     that membership, memberships are never hard-deleted while referenced,
--     and another module's rows must never vanish through a cascade it does
--     not own (users are global identities, ADR 0004).

-- owner: platform
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- owner: identity
-- Identity baseline (ADR 0004): users are global identities and join tenants
-- through memberships. Credentials (user_passwords) and external identities
-- (user_identities) live outside users; sessions are first-class and own a
-- rotating refresh-token family (RFC 9700 reuse detection).
--
-- FK actions:
--   * tenants are never hard-deleted (their lifecycle is a status), so every
--     tenants reference is RESTRICT/NO ACTION;
--   * users are soft-deleted (status = 'deleted'); rows fully owned by one user
--     (password, external identities, reset/verification tokens) CASCADE;
--   * memberships are business records referenced by other modules
--     (files, notifications, api_keys, ...), so both of their parents RESTRICT;
--   * sessions are fully owned by their membership and refresh tokens by their
--     session, so those CASCADE.

-- ---------------------------------------------------------------------------
-- identity: tenants
--
-- The default tenant is pinned to id 1 because every tenant_id column defaults
-- to 1. The identity sequence is then advanced past it so the next tenant
-- gets id 2 instead of colliding with the seed row.
--
-- Lifecycle: trialing -> active | suspended | pending_deletion;
-- active <-> suspended; active | suspended -> pending_deletion;
-- pending_deletion -> active (cancelled) | deleted. 'deleted' is terminal.
-- Only 'trialing' and 'active' tenants serve requests. The transitions are
-- enforced by the identity tenant service; the CHECKs below pin the
-- timestamps each state requires.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenants (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug VARCHAR(63) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('trialing', 'active', 'suspended', 'pending_deletion', 'deleted')),
    status_changed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    trial_ends_at TIMESTAMP WITH TIME ZONE,
    deletion_scheduled_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_tenants_trial_ends CHECK (status <> 'trialing' OR trial_ends_at IS NOT NULL),
    CONSTRAINT chk_tenants_deletion_scheduled
        CHECK ((status IN ('pending_deletion', 'deleted')) = (deletion_scheduled_at IS NOT NULL)),
    -- The compatibility tenant backs every tenant_id DEFAULT 1 column.
    CONSTRAINT chk_tenants_default_not_deleted
        CHECK (id <> 1 OR status NOT IN ('pending_deletion', 'deleted'))
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
-- identity: tenant settings (one row per tenant, typed columns only)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenant_settings (
    tenant_id BIGINT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    -- BCP 47 language tag used for tenant-facing emails and defaults.
    locale VARCHAR(35) NOT NULL DEFAULT 'en'
        CHECK (locale ~ '^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$'),
    -- IANA zone name; validated by the service (time.LoadLocation) because a
    -- CHECK cannot consult the zone database immutably.
    timezone VARCHAR(64) NOT NULL DEFAULT 'UTC',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

INSERT INTO tenant_settings (tenant_id) VALUES (1) ON CONFLICT (tenant_id) DO NOTHING;

DROP TRIGGER IF EXISTS trigger_tenant_settings_updated_at ON tenant_settings;
CREATE TRIGGER trigger_tenant_settings_updated_at
    BEFORE UPDATE ON tenant_settings
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: users (global identities)
--
-- status: active | suspended (platform-level, P2b-2) | deleted (the user
-- deleted their own account). deleted_at is set exactly when deleted. A ban
-- is per tenant and lives on memberships.
-- token_version is bumped by every global credential event (password change
-- or reset, email change, logout-all, account deletion) and invalidates every
-- access token and session of the user in every tenant.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email VARCHAR(255) NOT NULL,
    name VARCHAR(255) NOT NULL,
    email_verified_at TIMESTAMP WITH TIME ZONE,
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'suspended', 'deleted')),
    token_version INTEGER NOT NULL DEFAULT 0,
    -- Last password/email change; reset links issued before it are dead.
    credentials_changed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    -- The service stores the normalized (trimmed, lower-case) address.
    CONSTRAINT chk_users_email_normalized CHECK (email = lower(btrim(email))),
    CONSTRAINT chk_users_deleted CHECK ((status = 'deleted') = (deleted_at IS NOT NULL))
);

-- One live account per address, globally; a deleted account frees it.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_live
    ON users (email)
    WHERE status <> 'deleted';

DROP TRIGGER IF EXISTS trigger_users_updated_at ON users;
CREATE TRIGGER trigger_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: local password credential (at most one per user)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_passwords (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    password_hash VARCHAR(255) NOT NULL,
    changed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

DROP TRIGGER IF EXISTS trigger_user_passwords_updated_at ON user_passwords;
CREATE TRIGGER trigger_user_passwords_updated_at
    BEFORE UPDATE ON user_passwords
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: external identities (OAuth). One subject belongs to one user and
-- a user links at most one identity per provider (a link is never replaced).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_identities (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider VARCHAR(20) NOT NULL CHECK (provider IN ('google')),
    subject VARCHAR(255) NOT NULL,
    -- Address the provider reported when the identity was linked.
    email VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_user_identities_provider_subject UNIQUE (provider, subject),
    CONSTRAINT uq_user_identities_user_provider UNIQUE (user_id, provider)
);

DROP TRIGGER IF EXISTS trigger_user_identities_updated_at ON user_identities;
CREATE TRIGGER trigger_user_identities_updated_at
    BEFORE UPDATE ON user_identities
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: memberships (user <-> tenant, N:N)
--
-- status: active | suspended (banned by a tenant admin) | removed (left or
-- removed from the tenant). There is no 'invited' state: a pending invite is
-- an invitations row (it can target an address with no account yet), and
-- accepting it creates or reactivates the membership. Rows are never hard
-- deleted while other modules reference (tenant_id, user_id).
--
-- role is transitional (P2b-2 replaces it with membership_roles); the id
-- surrogate key is what role assignments will reference.
-- credentials_revoked_at is set by a ban or removal and never cleared: API
-- keys created at or before it stay invalid after the member is restored.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS memberships (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role VARCHAR(50) NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'suspended', 'removed')),
    banned_at TIMESTAMP WITH TIME ZONE,
    removed_at TIMESTAMP WITH TIME ZONE,
    credentials_revoked_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- Parent key for every cross-module (tenant_id, user_id) foreign key.
    CONSTRAINT uq_memberships_tenant_user UNIQUE (tenant_id, user_id),
    CONSTRAINT chk_memberships_banned CHECK ((status = 'suspended') = (banned_at IS NOT NULL)),
    CONSTRAINT chk_memberships_removed CHECK ((status = 'removed') = (removed_at IS NOT NULL))
);

-- Per-user lookups: login across tenants, global account deletion, and the
-- users(id) FK check.
CREATE INDEX IF NOT EXISTS idx_memberships_user_id
    ON memberships (user_id);
-- Tenant member lists (cursor pagination by join time).
CREATE INDEX IF NOT EXISTS idx_memberships_tenant_created_at_user
    ON memberships (tenant_id, created_at DESC, user_id DESC);
-- EmailBannedInTenant (public registration / invitations / email changes).
CREATE INDEX IF NOT EXISTS idx_memberships_tenant_suspended
    ON memberships (tenant_id, user_id)
    WHERE status = 'suspended';
-- CountActiveAdmins and the last-admin trigger.
CREATE INDEX IF NOT EXISTS idx_memberships_tenant_active_admins
    ON memberships (tenant_id, user_id)
    WHERE role = 'admin' AND status = 'active';

DROP TRIGGER IF EXISTS trigger_memberships_updated_at ON memberships;
CREATE TRIGGER trigger_memberships_updated_at
    BEFORE UPDATE ON memberships
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- A tenant keeps at least one active admin membership, even under concurrent
-- demote/ban/remove requests. Counting and locking are scoped to
-- OLD.tenant_id so one tenant's admins cannot satisfy another's; the other
-- admins are locked in user_id order. Identity services also take a
-- per-tenant advisory lock (LockTenantAdminChanges) before touching an admin
-- membership and return a friendly 409; a deadlock (40P01) or this trigger's
-- exception (P0001) from any other writer is mapped to 409 by the identity
-- repository.
CREATE OR REPLACE FUNCTION protect_last_admin()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.role = 'admin'
       AND OLD.status = 'active'
       AND (TG_OP = 'DELETE' OR NEW.role <> 'admin' OR NEW.status <> 'active'
            OR NEW.tenant_id <> OLD.tenant_id) THEN
        PERFORM 1
        FROM memberships
        WHERE tenant_id = OLD.tenant_id
          AND role = 'admin'
          AND status = 'active'
          AND user_id <> OLD.user_id
        ORDER BY user_id
        FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'cannot remove the last active admin';
        END IF;
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_protect_last_admin ON memberships;
CREATE TRIGGER trigger_protect_last_admin
    BEFORE UPDATE OF role, status, tenant_id OR DELETE ON memberships
    FOR EACH ROW
    EXECUTE FUNCTION protect_last_admin();

-- ---------------------------------------------------------------------------
-- identity: invitations (hashed single-use token)
--
-- One pending invitation per (tenant, address); re-inviting refreshes it.
-- Revoking deletes the pending row (the audit trail is audit_events);
-- accepting stamps accepted_at/accepted_by in the same transaction that
-- creates or reactivates the membership.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS invitations (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id) ON DELETE RESTRICT,
    email_normalized VARCHAR(255) NOT NULL,
    role VARCHAR(50) NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    token_hash VARCHAR(64) NOT NULL UNIQUE,
    invited_by BIGINT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    accepted_at TIMESTAMP WITH TIME ZONE,
    accepted_by BIGINT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_invitations_email_normalized
        CHECK (email_normalized = lower(btrim(email_normalized))),
    CONSTRAINT chk_invitations_accepted CHECK ((accepted_at IS NULL) = (accepted_by IS NULL)),
    CONSTRAINT fk_invitations_inviter FOREIGN KEY (tenant_id, invited_by)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_invitations_acceptor FOREIGN KEY (tenant_id, accepted_by)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_invitations_tenant_email_pending
    ON invitations (tenant_id, email_normalized)
    WHERE accepted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_invitations_tenant_created_at_id
    ON invitations (tenant_id, created_at DESC, id DESC);
-- The FK checks on the two membership references.
CREATE INDEX IF NOT EXISTS idx_invitations_tenant_invited_by
    ON invitations (tenant_id, invited_by);
CREATE INDEX IF NOT EXISTS idx_invitations_tenant_accepted_by
    ON invitations (tenant_id, accepted_by)
    WHERE accepted_by IS NOT NULL;
-- The expired-invitation purge.
CREATE INDEX IF NOT EXISTS idx_invitations_expires_at_pending
    ON invitations (expires_at)
    WHERE accepted_at IS NULL;

DROP TRIGGER IF EXISTS trigger_invitations_updated_at ON invitations;
CREATE TRIGGER trigger_invitations_updated_at
    BEFORE UPDATE ON invitations
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: sessions (one per sign-in; the refresh-token family)
--
-- A session is the RFC 9700 token family: every rotated refresh token
-- references it, and presenting an already-used token revokes it
-- (revoked_reason = 'refresh_reuse'). token_version is the user's version
-- when the session was created; a later credential event makes it stale.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sessions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    user_id BIGINT NOT NULL,
    token_version INTEGER NOT NULL,
    user_agent VARCHAR(512),
    ip INET,
    -- Authentication methods used to establish the session (RFC 8176). A
    -- refreshed access token repeats them; recovery codes record 'rc'.
    amr TEXT[] NOT NULL DEFAULT ARRAY['pwd']::TEXT[],
    last_used_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMP WITH TIME ZONE,
    revoked_reason VARCHAR(30)
        CHECK (revoked_reason IN ('logout', 'logout_all', 'revoked', 'refresh_reuse',
                                  'credentials_changed', 'membership_changed', 'account_deleted',
                                  'mfa_changed')),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_sessions_revoked CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    -- Parent key for refresh_tokens (tenant_id, session_id).
    CONSTRAINT uq_sessions_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT fk_sessions_membership FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE
);

-- "My sessions" in the request tenant and per-tenant revocation.
CREATE INDEX IF NOT EXISTS idx_sessions_tenant_user_created_at
    ON sessions (tenant_id, user_id, created_at DESC);
-- Revoking every live session of a user across tenants (logout-all,
-- credential changes, account deletion).
CREATE INDEX IF NOT EXISTS idx_sessions_user_live
    ON sessions (user_id)
    WHERE revoked_at IS NULL;
-- The session purge (revoked or idle sessions without live refresh tokens).
CREATE INDEX IF NOT EXISTS idx_sessions_last_used_at
    ON sessions (last_used_at);

DROP TRIGGER IF EXISTS trigger_sessions_updated_at ON sessions;
CREATE TRIGGER trigger_sessions_updated_at
    BEFORE UPDATE ON sessions
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: refresh tokens (SHA-256 hashes, never plaintext)
--
-- Append-only apart from used_at, which is the token's own timestamp, so the
-- table has no updated_at. Used tokens are kept until they expire so a replay
-- is recognized as reuse instead of an unknown token.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    session_id BIGINT NOT NULL,
    token_hash VARCHAR(64) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_refresh_tokens_session FOREIGN KEY (tenant_id, session_id)
        REFERENCES sessions (tenant_id, id) ON DELETE CASCADE
);

-- The FK cascade and the session purge's "no live token" check.
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_tenant_session_id
    ON refresh_tokens (tenant_id, session_id);
-- The scheduled cross-tenant purge deletes expired rows in bounded batches.
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at
    ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- identity: password-reset tokens (one live token per user)
--
-- Per user, not per tenant: the password is a global credential. The emailed
-- link may still carry ?tenant=<slug> so the frontend knows where to sign in
-- afterwards; the API ignores it for the lookup.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    token_hash VARCHAR(64) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- identity: email-verification tokens (bound to the target address)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_verification_tokens (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    token_hash VARCHAR(64) NOT NULL UNIQUE,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- identity: MFA factors (ADR 0010)
--
-- MFA is per global user, not per tenant. The TOTP secret is sealed with the
-- data encryption keyring (DATA_ENCRYPTION_KEY) and never returned again after
-- enrollment. last_used_step is the RFC 6238 step of the last accepted code;
-- the verifier updates it with last_used_step < step so one code cannot be
-- replayed, including by concurrent requests.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mfa_factors (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type VARCHAR(20) NOT NULL CHECK (type IN ('totp')),
    secret_ciphertext TEXT NOT NULL,
    confirmed_at TIMESTAMP WITH TIME ZONE,
    last_used_step BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- At most one confirmed factor of each type per user, and one pending
-- enrollment (upsert target for re-enrollment).
CREATE UNIQUE INDEX IF NOT EXISTS idx_mfa_factors_user_confirmed_totp
    ON mfa_factors (user_id)
    WHERE type = 'totp' AND confirmed_at IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_mfa_factors_user_unconfirmed_totp
    ON mfa_factors (user_id)
    WHERE type = 'totp' AND confirmed_at IS NULL;

DROP TRIGGER IF EXISTS trigger_mfa_factors_updated_at ON mfa_factors;
CREATE TRIGGER trigger_mfa_factors_updated_at
    BEFORE UPDATE ON mfa_factors
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: single-use recovery codes (SHA-256 hashes, never plaintext)
--
-- Ten codes are issued on confirmation and on regenerate; used_at marks the
-- one that was consumed, so a code can never authenticate twice.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mfa_recovery_codes (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash VARCHAR(64) NOT NULL,
    used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_mfa_recovery_codes_user_hash UNIQUE (user_id, code_hash)
);

CREATE INDEX IF NOT EXISTS idx_mfa_recovery_codes_user_unused
    ON mfa_recovery_codes (user_id)
    WHERE used_at IS NULL;

-- owner: platform
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
        CHECK (status <> 'running' OR lease_expires_at IS NOT NULL),
    -- Exactly the running rows have an owner: the claim sets locked_by and
    -- every transition out of running (succeed, fail, release, lease-expiry
    -- requeue) clears it, so ownership fences can never match a queued or
    -- terminal row.
    CONSTRAINT jobs_running_has_owner
        CHECK ((status = 'running') = (locked_by IS NOT NULL))
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

-- owner: platform
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

-- owner: platform
-- ---------------------------------------------------------------------------
-- platform: audit trail
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    -- The intent row a domain event belongs to. Audit rows are never deleted
    -- (see the triggers below), so the default NO ACTION is never exercised.
    parent_id BIGINT REFERENCES audit_events(id),
    actor_id BIGINT,
    -- What kind of principal acted. The principal's role (user/admin/...) is
    -- actor_role; it is not repeated here.
    actor_type TEXT NOT NULL DEFAULT 'unknown'
        CHECK (actor_type IN ('user', 'service', 'job', 'system', 'anonymous', 'unknown')),
    actor_role TEXT,
    action TEXT NOT NULL,
    resource TEXT NOT NULL,
    resource_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Client address. The recorder stores NULL for anything that does not
    -- parse as an IP address (zone stripped), never the raw header value.
    ip INET,
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

-- owner: apikeys
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

-- owner: files
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
    size BIGINT NOT NULL CHECK (size >= 0),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    -- files.trash.purge claim: set before the blob is deleted; restore requires NULL.
    purge_started_at TIMESTAMP WITH TIME ZONE,
    CONSTRAINT fk_files_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

-- Deliberately NOT partial: it serves ListFilesByUserCursor for both
-- include_deleted values under a generic plan, ListFilesByUserID and
-- CountFilesByUserID (deleted_at is a cheap heap filter), and the
-- fk_files_tenant_user check when a membership row is deleted or re-keyed.
CREATE INDEX IF NOT EXISTS idx_files_tenant_user_created
    ON files (tenant_id, user_id, created_at DESC, id DESC);
-- AdminListFilesCursor / AdminListFiles / AdminFileStats.
CREATE INDEX IF NOT EXISTS idx_files_tenant_created_at_id
    ON files (tenant_id, created_at DESC, id DESC);
-- files.trash.purge scans every tenant by age, so this leads with deleted_at.
CREATE INDEX IF NOT EXISTS idx_files_deleted_at
    ON files (deleted_at)
    WHERE deleted_at IS NOT NULL;

DROP TRIGGER IF EXISTS trigger_files_updated_at ON files;
CREATE TRIGGER trigger_files_updated_at
    BEFORE UPDATE ON files
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- owner: features
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

-- owner: notifications
-- ---------------------------------------------------------------------------
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
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

-- The non-partial index also serves the fk_notifications_tenant_user check.
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_user_created_at_id
    ON notifications (tenant_id, user_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_notifications_tenant_user_unread
    ON notifications (tenant_id, user_id, created_at DESC, id DESC)
    WHERE read_at IS NULL;
-- notifications.retention.purge scans every tenant by read_at.
CREATE INDEX IF NOT EXISTS idx_notifications_read_at
    ON notifications (read_at)
    WHERE read_at IS NOT NULL;

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
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT
);

-- Also serves the fk_notification_preferences_tenant_user check.
CREATE UNIQUE INDEX IF NOT EXISTS idx_notification_preferences_unique
    ON notification_preferences (tenant_id, user_id, type, channel);

DROP TRIGGER IF EXISTS trigger_notification_preferences_updated_at ON notification_preferences;
CREATE TRIGGER trigger_notification_preferences_updated_at
    BEFORE UPDATE ON notification_preferences
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- owner: webhooks
-- ---------------------------------------------------------------------------
-- webhooks: outbound endpoints, their event subscriptions, and versioned
-- signing secrets
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

-- Parent key for the tenant-composite FKs from subscriptions, secrets, and
-- deliveries.
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

-- One row per event-type pattern an endpoint subscribes to: an exact type
-- ("file.uploaded"), a prefix wildcard ("file.*"), or "*". Matching happens in
-- the dispatcher (webhookcontracts.Match); the patterns are validated there
-- too. Subscriptions are configuration fully owned by their endpoint, so
-- CASCADE; endpoints are soft-deleted, so this only fires on a hard delete.
CREATE TABLE IF NOT EXISTS webhook_endpoint_subscriptions (
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    endpoint_id BIGINT NOT NULL,
    pattern VARCHAR(128) NOT NULL CHECK (pattern <> ''),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (endpoint_id, pattern),
    CONSTRAINT fk_webhook_endpoint_subscriptions_tenant_endpoint FOREIGN KEY (tenant_id, endpoint_id)
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE CASCADE
);

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
-- webhooks: outbound events, delivery outbox, and delivery attempts
--
-- webhook_events holds each dispatched event once: its id, type, occurrence
-- time, and payload are the signed envelope every delivery of it sends. The
-- dispatch job writes the event row and its first delivery row (plus the
-- delivery's durable job) in one transaction; later endpoints of the same
-- event reuse the row. A replay is a new event (new id and createdAt) with a
-- copy of the original payload.
--
-- A delivery's url/secret_ciphertext/timeout_ms/max_attempts are snapshots
-- taken from the endpoint at dispatch: editing or rotating an endpoint cannot
-- change a queued delivery's signed request. event_id is deduplicated per
-- endpoint so re-dispatching the same domain event is safe.
-- ---------------------------------------------------------------------------
-- The columns are the versioned event envelope (ADR 0011): event_id (UUIDv7),
-- event_type, data_version, occurred_at, tenant_id, actor_type/actor_id, and
-- payload (the envelope's data). The delivery worker rebuilds the signed
-- JSON body from them.
CREATE TABLE IF NOT EXISTS webhook_events (
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    event_id VARCHAR(128) NOT NULL,
    event_type VARCHAR(128) NOT NULL,
    data_version INTEGER NOT NULL CHECK (data_version > 0),
    actor_type VARCHAR(20) NOT NULL
        CHECK (actor_type IN ('user', 'service', 'system')),
    -- A system actor has no id; user and service actors always have one.
    actor_id VARCHAR(128)
        CHECK ((actor_type = 'system') = (actor_id IS NULL)),
    occurred_at TIMESTAMP WITH TIME ZONE NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, event_id)
);

-- Cross-tenant retention pass (PurgeOrphanWebhookEvents): oldest events first.
CREATE INDEX IF NOT EXISTS idx_webhook_events_created_at
    ON webhook_events (created_at);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id),
    -- Deliveries are history: endpoints are soft-deleted and a hard delete
    -- must not silently erase the delivery log (RESTRICT, tenant-composite).
    endpoint_id BIGINT NOT NULL,
    -- RESTRICT: an event is purged only after every delivery of it is.
    event_id VARCHAR(128) NOT NULL,
    url VARCHAR(2048) NOT NULL,
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
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_webhook_deliveries_tenant_event FOREIGN KEY (tenant_id, event_id)
        REFERENCES webhook_events (tenant_id, event_id) ON DELETE RESTRICT
);

-- Parent key for the tenant-composite FK from webhook_delivery_attempts.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_id_id
    ON webhook_deliveries (tenant_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_endpoint_event
    ON webhook_deliveries (tenant_id, endpoint_id, event_id);
-- Serves the fk_webhook_deliveries_tenant_event check when an event is
-- deleted and the NOT EXISTS probe of PurgeOrphanWebhookEvents.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant_event
    ON webhook_deliveries (tenant_id, event_id);
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
    delivery_id BIGINT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    status_code INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- Tenant-composite so an attempt can never carry a different tenant than
    -- its delivery (parent key: idx_webhook_deliveries_tenant_id_id). CASCADE
    -- by design: attempts are fully owned by their delivery, and the batched
    -- retention purge (PurgeWebhookDeliveries) relies on it to remove a
    -- delivery's attempts in the same statement.
    CONSTRAINT fk_webhook_delivery_attempts_tenant_delivery FOREIGN KEY (tenant_id, delivery_id)
        REFERENCES webhook_deliveries (tenant_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_delivery_attempts_delivery_attempt
    ON webhook_delivery_attempts (delivery_id, attempt);
-- Tenant-leading index for ListWebhookDeliveryAttempts; it also serves the
-- fk_webhook_delivery_attempts_tenant_delivery cascade. Attempts have no
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
