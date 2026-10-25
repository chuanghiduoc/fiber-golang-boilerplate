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
    last_used_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMP WITH TIME ZONE,
    revoked_reason VARCHAR(30)
        CHECK (revoked_reason IN ('logout', 'logout_all', 'revoked', 'refresh_reuse',
                                  'credentials_changed', 'membership_changed', 'account_deleted')),
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
