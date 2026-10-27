-- Fresh-install schema: the single init migration. Each section
-- starts with an `-- owner: <module|platform>` marker, and test/arch checks
-- that a section only creates its owner's tables. Sections run in foreign-key
-- dependency order: shared functions, identity, platform tables, modules.
-- Every later change is a new <UTC timestamp>_<owner>_<purpose> migration;
-- this file is edited in place only until the first release.
--
-- Conventions:
--   * one owner per table; the section marker names the owner;
--   * every identifier is a UUIDv7: primary keys default to the PostgreSQL 18
--     native uuidv7() (Go code that needs an id before insert calls
--     uuid.NewV7()); every foreign key/reference column is UUID;
--   * no BIGINT identity columns and no surrogate id where the natural key is
--     the row's identity (notification_preferences);
--   * tenant-owned columns have no default: every row states its tenant;
--   * active-row uniqueness uses partial indexes (soft delete friendly);
--   * cursor pagination is backed by (created_at DESC, id DESC) indexes;
--   * text columns are TEXT with an explicit length CHECK (never VARCHAR);
--   * hashes and digests are BYTEA with an exact 32-byte (SHA-256) CHECK;
--   * constraint names follow one convention: pk_/fk_/uq_/chk_/idx_;
--   * timestamps are NOT NULL so pagination cannot hide rows;
--   * every tenant-owned row carries tenant_id and tenant-leading indexes;
--   * a cross-module per-user row references memberships (tenant_id, user_id)
--     with ON DELETE RESTRICT: a module row for a user in a tenant requires
--     that membership, memberships are never hard-deleted while referenced,
--     and another module's rows must never vanish through a cascade it does
--     not own (users are global identities).

-- owner: platform
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- app_tenant_visible is the single RLS predicate for tenant-owned tables. It
-- reads only the transaction-local app.tenant_id that TxManager/Scoped
-- install, so a pooled connection is never scoped to its previous caller.
-- With no setting, the comparison against NULL is NULL, so the row is
-- invisible (fail closed). Cross-tenant work uses a separate role with
-- BYPASSRLS (a role property the application role cannot acquire); reading an
-- application-writable GUC here would let any injected statement escape RLS.
CREATE OR REPLACE FUNCTION app_tenant_visible(row_tenant UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
  SELECT row_tenant = NULLIF(current_setting('app.tenant_id', true), '')::uuid
$$;

-- owner: identity
-- Identity baseline: users are global identities and join tenants
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
-- The default tenant is pinned to a documented UUIDv7 constant as the tenant
-- of the default slug. It is a real tenant row, not a compatibility fallback:
-- tenant-owned columns have no default. The same constant is tenant.DefaultID
-- in Go.
--
-- Lifecycle: trialing -> active | suspended | pending_deletion;
-- active <-> suspended; active | suspended -> pending_deletion;
-- pending_deletion -> active (cancelled) | deleted. 'deleted' is terminal.
-- Only 'trialing' and 'active' tenants serve requests. The transitions are
-- enforced by the identity tenant service; the CHECKs below pin the
-- timestamps each state requires.
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
    id UUID DEFAULT uuidv7(),
    slug TEXT NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    status_changed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    trial_ends_at TIMESTAMP WITH TIME ZONE,
    deletion_scheduled_at TIMESTAMP WITH TIME ZONE,
    -- Set by identity.tenants.purge_deleted once every membership's per-user
    -- data and every module's tenant-wide rows are gone. The tenants row and
    -- its audit trail stay forever; this marker keeps the job idempotent.
    purged_at TIMESTAMP WITH TIME ZONE,
    -- Purge progress survives a run (and a worker restart): the last
    -- membership user id whose erasers succeeded. User ids are UUIDv7, so the
    -- ordering is total and a resumed pass continues after this value.
    -- purge_progress_at orders unfinished tenants by least recent progress,
    -- so one large tenant cannot starve the rest.
    purge_cursor UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    purge_progress_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_tenants PRIMARY KEY (id),
    CONSTRAINT uq_tenants_slug UNIQUE (slug),
    CONSTRAINT chk_tenants_slug_len CHECK (length(slug) <= 63),
    CONSTRAINT chk_tenants_name_len CHECK (length(name) <= 255),
    CONSTRAINT chk_tenants_status
        CHECK (status IN ('trialing', 'active', 'suspended', 'pending_deletion', 'deleted')),
    CONSTRAINT chk_tenants_trial_ends CHECK (status <> 'trialing' OR trial_ends_at IS NOT NULL),
    CONSTRAINT chk_tenants_deletion_scheduled
        CHECK ((status IN ('pending_deletion', 'deleted')) = (deletion_scheduled_at IS NOT NULL)),
    CONSTRAINT chk_tenants_purged CHECK (status = 'deleted' OR purged_at IS NULL),
    -- Progress is only meaningful while a deleted tenant is being purged.
    CONSTRAINT chk_tenants_purge_progress
        CHECK (status = 'deleted'
               OR (purge_cursor = '00000000-0000-0000-0000-000000000000' AND purge_progress_at IS NULL)),
    -- The seeded default tenant is a real tenant row. Tenant-owned tables
    -- have no tenant_id default: every row must state its tenant explicitly.
    CONSTRAINT chk_tenants_default_not_deleted
        CHECK (id <> '0192c9b0-7e4a-7000-8000-000000000001'
               OR status NOT IN ('pending_deletion', 'deleted'))
);

INSERT INTO tenants (id, slug, name)
VALUES ('0192c9b0-7e4a-7000-8000-000000000001', 'default', 'Default');

-- Platform tenant listing (cursor pagination by creation time).
CREATE INDEX idx_tenants_created_at_id
    ON tenants (created_at DESC, id DESC);
-- The deleted-tenant purge scans only unfinished deletions, least recently
-- advanced first so every tenant gets a turn. NULLS FIRST matches the query's
-- order for tenants that have not made any progress yet.
CREATE INDEX idx_tenants_deleted_unpurged
    ON tenants (purge_progress_at ASC NULLS FIRST, id)
    WHERE status = 'deleted' AND purged_at IS NULL;
-- Pending deletions whose grace has ended (the hourly finalize job).
CREATE INDEX idx_tenants_deletion_due
    ON tenants (deletion_scheduled_at, id)
    WHERE status = 'pending_deletion';

CREATE TRIGGER trigger_tenants_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: tenant settings (one row per tenant, typed columns only)
-- ---------------------------------------------------------------------------
CREATE TABLE tenant_settings (
    tenant_id UUID NOT NULL,
    -- BCP 47 language tag used for tenant-facing emails and defaults.
    locale TEXT NOT NULL DEFAULT 'en',
    -- IANA zone name; validated by the service (time.LoadLocation) because a
    -- CHECK cannot consult the zone database immutably.
    timezone TEXT NOT NULL DEFAULT 'UTC',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_tenant_settings PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_settings_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT chk_tenant_settings_locale_len CHECK (length(locale) <= 35),
    CONSTRAINT chk_tenant_settings_locale
        CHECK (locale ~ '^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$'),
    CONSTRAINT chk_tenant_settings_timezone_len CHECK (length(timezone) <= 64)
);

INSERT INTO tenant_settings (tenant_id) VALUES ('0192c9b0-7e4a-7000-8000-000000000001');

CREATE TRIGGER trigger_tenant_settings_updated_at
    BEFORE UPDATE ON tenant_settings
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: users (global identities)
--
-- status: active | deleted (the user deleted their own account). deleted_at is
-- set exactly when deleted. A ban is per tenant and lives on memberships.
-- token_version is bumped by every global credential event (password change
-- or reset, email change, logout-all, account deletion) and invalidates every
-- access token and session of the user in every tenant.
-- ---------------------------------------------------------------------------
CREATE TABLE users (
    id UUID DEFAULT uuidv7(),
    email TEXT NOT NULL,
    name TEXT NOT NULL,
    email_verified_at TIMESTAMP WITH TIME ZONE,
    status TEXT NOT NULL DEFAULT 'active',
    token_version INTEGER NOT NULL DEFAULT 0,
    -- Last password/email change; reset links issued before it are dead.
    credentials_changed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    CONSTRAINT pk_users PRIMARY KEY (id),
    CONSTRAINT chk_users_email_len CHECK (length(email) <= 255),
    CONSTRAINT chk_users_name_len CHECK (length(name) <= 255),
    CONSTRAINT chk_users_status CHECK (status IN ('active', 'deleted')),
    -- The service stores the normalized (trimmed, lower-case) address.
    CONSTRAINT chk_users_email_normalized CHECK (email = lower(btrim(email))),
    CONSTRAINT chk_users_deleted CHECK ((status = 'deleted') = (deleted_at IS NOT NULL))
);

-- One live account per address, globally; a deleted account frees it.
CREATE UNIQUE INDEX idx_users_email_live
    ON users (email)
    WHERE status <> 'deleted';

CREATE TRIGGER trigger_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: local password credential (at most one per user)
-- ---------------------------------------------------------------------------
CREATE TABLE user_passwords (
    user_id UUID NOT NULL,
    password_hash TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_user_passwords PRIMARY KEY (user_id),
    CONSTRAINT fk_user_passwords_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT chk_user_passwords_hash_len CHECK (length(password_hash) <= 255)
);

CREATE TRIGGER trigger_user_passwords_updated_at
    BEFORE UPDATE ON user_passwords
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: external identities (OAuth). One subject belongs to one user and
-- a user links at most one identity per provider (a link is never replaced).
-- ---------------------------------------------------------------------------
CREATE TABLE user_identities (
    id UUID DEFAULT uuidv7(),
    user_id UUID NOT NULL,
    provider TEXT NOT NULL,
    subject TEXT NOT NULL,
    -- Address the provider reported when the identity was linked.
    email TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_user_identities PRIMARY KEY (id),
    CONSTRAINT fk_user_identities_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_user_identities_provider_subject UNIQUE (provider, subject),
    CONSTRAINT uq_user_identities_user_provider UNIQUE (user_id, provider),
    CONSTRAINT chk_user_identities_provider_len CHECK (length(provider) <= 20),
    CONSTRAINT chk_user_identities_provider CHECK (provider IN ('google')),
    CONSTRAINT chk_user_identities_subject_len CHECK (length(subject) <= 255),
    CONSTRAINT chk_user_identities_email_len CHECK (length(email) <= 255)
);

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
-- Roles live in membership_roles.
-- credentials_revoked_at is set by a ban or removal and never cleared: API
-- keys created at or before it stay invalid after the member is restored.
-- banned_email_hash is a keyed pseudonymous marker (HMAC-SHA256 with a key
-- derived from the data-encryption key set), computed in Go and written by a
-- ban so EmailBannedInTenant still refuses the address after an erasure
-- rewrites users.email. banned_email_hash_kid names the key that wrote it; a
-- lookup computes the marker under every configured key (active and previous).
-- It is retained with banned_at on a removed membership (GDPR Art.17(3)(e),
-- defence of the tenant's ban decision).
-- ---------------------------------------------------------------------------
CREATE TABLE memberships (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    banned_at TIMESTAMP WITH TIME ZONE,
    banned_email_hash BYTEA,
    banned_email_hash_kid TEXT,
    -- Set when an erasure request suspends this membership. It reserves the
    -- membership (and its sessions/API keys stop working) until privacy.erase
    -- removes it; unlike banned_at it carries no ban semantics, so erasure
    -- never blocks the address from re-registering later.
    erasure_requested_at TIMESTAMP WITH TIME ZONE,
    removed_at TIMESTAMP WITH TIME ZONE,
    credentials_revoked_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_memberships PRIMARY KEY (id),
    CONSTRAINT fk_memberships_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id) ON DELETE RESTRICT,
    CONSTRAINT fk_memberships_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE RESTRICT,
    -- Parent key for every cross-module (tenant_id, user_id) foreign key.
    CONSTRAINT uq_memberships_tenant_user UNIQUE (tenant_id, user_id),
    CONSTRAINT chk_memberships_status CHECK (status IN ('active', 'suspended', 'removed')),
    CONSTRAINT chk_memberships_ban_marker CHECK (
        (banned_email_hash IS NULL) = (banned_email_hash_kid IS NULL)
    ),
    CONSTRAINT chk_memberships_banned_email_hash_len CHECK (
        banned_email_hash IS NULL OR octet_length(banned_email_hash) = 32
    ),
    CONSTRAINT chk_memberships_banned_email_hash_kid CHECK (
        banned_email_hash_kid IS NULL OR banned_email_hash_kid ~ '^[0-9a-f]{8}$'
    ),
    -- An active membership carries neither marker. A suspended one is either
    -- banned, pending erasure, or both (erasing a banned member keeps the ban).
    -- A removed membership may keep either marker for its provenance.
    CONSTRAINT chk_memberships_banned CHECK (
        (status = 'active' AND banned_at IS NULL AND erasure_requested_at IS NULL)
        OR (status = 'suspended' AND (banned_at IS NOT NULL OR erasure_requested_at IS NOT NULL))
        OR (status = 'removed')
    ),
    CONSTRAINT chk_memberships_removed CHECK ((status = 'removed') = (removed_at IS NOT NULL))
);

-- Per-user lookups: login across tenants, global account deletion, and the
-- users(id) FK check.
CREATE INDEX idx_memberships_user_id
    ON memberships (user_id);
-- Tenant member lists (cursor pagination by join time).
CREATE INDEX idx_memberships_tenant_created_at_user
    ON memberships (tenant_id, created_at DESC, user_id DESC);
-- EmailBannedInTenant's keyed match (registration / invitations / email
-- changes), which must keep a ban after the membership was removed by an
-- erasure. A lookup passes the marker for the active and every previous key.
CREATE INDEX idx_memberships_tenant_banned_email_hash
    ON memberships (tenant_id, banned_email_hash)
    WHERE banned_email_hash IS NOT NULL;

CREATE TRIGGER trigger_memberships_updated_at
    BEFORE UPDATE ON memberships
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: RBAC
--
-- The permission catalog is code-owned and synchronized into permissions at
-- API startup. Roles are rows: system roles have tenant_id IS NULL (seeded
-- here, immutable to tenants); tenant roles belong to one tenant. Membership
-- roles are tenant-scoped; platform role assignments are the only source of
-- cross-tenant access.
-- ---------------------------------------------------------------------------
CREATE TABLE permissions (
    key TEXT NOT NULL,
    owner TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_permissions PRIMARY KEY (key),
    CONSTRAINT chk_permissions_key
        CHECK (key ~ '^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)+$')
);

CREATE TRIGGER trigger_permissions_updated_at
    BEFORE UPDATE ON permissions
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE roles (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID,
    scope TEXT NOT NULL,
    key TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_roles PRIMARY KEY (id),
    CONSTRAINT fk_roles_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id) ON DELETE RESTRICT,
    CONSTRAINT chk_roles_scope_len CHECK (length(scope) <= 20),
    CONSTRAINT chk_roles_scope CHECK (scope IN ('tenant', 'platform')),
    CONSTRAINT chk_roles_key_len CHECK (length(key) <= 50),
    CONSTRAINT chk_roles_key CHECK (key ~ '^[a-z][a-z0-9_]*$'),
    CONSTRAINT chk_roles_name_len CHECK (length(name) <= 255),
    -- NULLS NOT DISTINCT makes the system roles (tenant_id NULL) unique by key.
    CONSTRAINT uq_roles_tenant_key UNIQUE NULLS NOT DISTINCT (tenant_id, key),
    -- A platform role is never owned by a tenant.
    CONSTRAINT chk_roles_platform_tenant CHECK (scope <> 'platform' OR tenant_id IS NULL),
    -- A tenant role may not shadow a system role key: assignment by key must
    -- never resolve to a different role than the caller listed.
    CONSTRAINT chk_roles_tenant_key_system
        CHECK (tenant_id IS NULL OR key NOT IN ('owner', 'admin', 'member', 'platform_admin'))
);

-- System-role listing (small partial index) and tenant role pagination.
CREATE INDEX idx_roles_system_created_at_id
    ON roles (created_at DESC, id DESC)
    WHERE tenant_id IS NULL;
CREATE INDEX idx_roles_tenant_created_at_id
    ON roles (tenant_id, created_at DESC, id DESC)
    WHERE tenant_id IS NOT NULL;

INSERT INTO roles (tenant_id, scope, key, name)
VALUES (NULL, 'tenant', 'owner', 'Owner'),
       (NULL, 'tenant', 'admin', 'Admin'),
       (NULL, 'tenant', 'member', 'Member'),
       (NULL, 'platform', 'platform_admin', 'Platform Admin');

CREATE TRIGGER trigger_roles_updated_at
    BEFORE UPDATE ON roles
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE role_permissions (
    role_id UUID NOT NULL,
    permission_key TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_role_permissions PRIMARY KEY (role_id, permission_key),
    CONSTRAINT fk_role_permissions_role FOREIGN KEY (role_id)
        REFERENCES roles (id) ON DELETE CASCADE,
    CONSTRAINT fk_role_permissions_permission FOREIGN KEY (permission_key)
        REFERENCES permissions (key) ON DELETE RESTRICT
);

-- Deleting a permission (catalog drift) and the startup sync's prune.
CREATE INDEX idx_role_permissions_permission
    ON role_permissions (permission_key);

-- Tenant roles may only hold tenant-scoped permissions. Every platform
-- permission uses the reserved `platform:` prefix (authz.Grant.Platform).
--
-- The lookup runs under the caller's RLS: a role owned by another tenant is
-- invisible, so it must raise instead of reading a NULL tenant_id (which would
-- otherwise look like a shared system role and allow platform permissions).
CREATE OR REPLACE FUNCTION validate_role_permission()
RETURNS TRIGGER AS $$
DECLARE
    owner_tenant UUID;
BEGIN
    SELECT tenant_id INTO owner_tenant FROM roles WHERE id = NEW.role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'role does not exist or is not visible to the caller';
    END IF;
    IF owner_tenant IS NOT NULL AND NEW.permission_key LIKE 'platform:%' THEN
        RAISE EXCEPTION 'tenant roles cannot hold platform permissions';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_role_permissions_scope
    BEFORE INSERT OR UPDATE ON role_permissions
    FOR EACH ROW
    EXECUTE FUNCTION validate_role_permission();

CREATE TABLE membership_roles (
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    role_id UUID NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_membership_roles PRIMARY KEY (tenant_id, user_id, role_id),
    CONSTRAINT fk_membership_roles_membership FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE,
    CONSTRAINT fk_membership_roles_role FOREIGN KEY (role_id)
        REFERENCES roles (id) ON DELETE RESTRICT
);

-- Replacing one member's roles (PK) and invalidating everyone holding a role.
CREATE INDEX idx_membership_roles_role
    ON membership_roles (role_id);

CREATE TABLE platform_role_assignments (
    user_id UUID NOT NULL,
    role_id UUID NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_platform_role_assignments PRIMARY KEY (user_id, role_id),
    CONSTRAINT fk_platform_role_assignments_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_platform_role_assignments_role FOREIGN KEY (role_id)
        REFERENCES roles (id) ON DELETE RESTRICT
);

CREATE INDEX idx_platform_role_assignments_role
    ON platform_role_assignments (role_id);

-- A membership may only carry a tenant role that is either a system role or
-- belongs to the membership's own tenant.
CREATE OR REPLACE FUNCTION validate_membership_role()
RETURNS TRIGGER AS $$
DECLARE
    role_row roles%ROWTYPE;
BEGIN
    SELECT * INTO role_row FROM roles WHERE id = NEW.role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'role does not exist';
    END IF;
    IF role_row.scope <> 'tenant' THEN
        RAISE EXCEPTION 'membership roles must be tenant-scoped';
    END IF;
    IF role_row.tenant_id IS NOT NULL AND role_row.tenant_id <> NEW.tenant_id THEN
        RAISE EXCEPTION 'role belongs to another tenant';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_membership_roles_scope
    BEFORE INSERT OR UPDATE ON membership_roles
    FOR EACH ROW
    EXECUTE FUNCTION validate_membership_role();

-- Platform assignments are the only cross-tenant grant; they require a
-- platform-scoped role.
CREATE OR REPLACE FUNCTION validate_platform_role()
RETURNS TRIGGER AS $$
DECLARE
    role_row roles%ROWTYPE;
BEGIN
    SELECT * INTO role_row FROM roles WHERE id = NEW.role_id;
    IF NOT FOUND OR role_row.scope <> 'platform' OR role_row.tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'platform assignments require a platform-scoped role';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_platform_role_scope
    BEFORE INSERT OR UPDATE ON platform_role_assignments
    FOR EACH ROW
    EXECUTE FUNCTION validate_platform_role();

-- An operational tenant keeps at least one active member holding the system
-- owner role. Owner removal is serialized per tenant with an advisory lock so
-- two concurrent removals cannot each see the other owner and both commit;
-- identity services take the same lock (LockTenantOwnerChanges) and return a
-- friendly 409. Non-operational tenants may have no owner.
CREATE OR REPLACE FUNCTION protect_last_owner()
RETURNS TRIGGER AS $$
DECLARE
    target_tenant UUID;
    owner_role UUID;
    operational BOOLEAN;
BEGIN
    target_tenant := OLD.tenant_id;
    SELECT id INTO owner_role
    FROM roles WHERE tenant_id IS NULL AND scope = 'tenant' AND key = 'owner';

    IF TG_TABLE_NAME = 'membership_roles' THEN
        IF TG_OP = 'UPDATE' AND NEW.role_id = OLD.role_id
           AND NEW.tenant_id = OLD.tenant_id AND NEW.user_id = OLD.user_id THEN
            RETURN NEW;
        END IF;
        IF OLD.role_id <> owner_role THEN
            RETURN COALESCE(NEW, OLD);
        END IF;
    ELSE
        IF OLD.status <> 'active' THEN
            RETURN COALESCE(NEW, OLD);
        END IF;
        IF TG_OP = 'UPDATE' AND NEW.status = 'active' AND NEW.tenant_id = OLD.tenant_id THEN
            RETURN NEW;
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM membership_roles
            WHERE tenant_id = OLD.tenant_id AND user_id = OLD.user_id
              AND role_id = owner_role
        ) THEN
            RETURN COALESCE(NEW, OLD);
        END IF;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM memberships m
        JOIN users u ON u.id = m.user_id
        WHERE m.tenant_id = OLD.tenant_id AND m.user_id = OLD.user_id
          AND m.status = 'active' AND u.status = 'active'
    ) THEN
        RETURN COALESCE(NEW, OLD);
    END IF;

    SELECT status IN ('trialing', 'active') INTO operational
    FROM tenants WHERE id = target_tenant;
    IF NOT COALESCE(operational, FALSE) THEN
        RETURN COALESCE(NEW, OLD);
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended('identity.last_owner:' || target_tenant, 0));

    PERFORM 1
    FROM membership_roles mr
    JOIN memberships m ON m.tenant_id = mr.tenant_id AND m.user_id = mr.user_id
    JOIN users u ON u.id = m.user_id
    WHERE mr.tenant_id = target_tenant
      AND mr.role_id = owner_role
      AND mr.user_id <> OLD.user_id
      AND m.status = 'active'
      AND u.status = 'active'
    LIMIT 1
    FOR UPDATE OF mr;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'cannot remove the last owner' USING ERRCODE = 'LO001';
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_protect_last_owner
    BEFORE DELETE OR UPDATE ON membership_roles
    FOR EACH ROW
    EXECUTE FUNCTION protect_last_owner();

CREATE TRIGGER trigger_memberships_last_owner
    BEFORE UPDATE OF status, tenant_id OR DELETE ON memberships
    FOR EACH ROW
    EXECUTE FUNCTION protect_last_owner();

-- ---------------------------------------------------------------------------
-- identity: data exports (GDPR right of access)
--
-- One row per requested tenant-scoped export. Active means queued/running; the
-- partial unique index allows one in-flight request per membership. The blob
-- key is only set when the export becomes ready; expires_at bounds retention.
-- ---------------------------------------------------------------------------
CREATE TABLE data_exports (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    storage_path TEXT,
    expires_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_data_exports PRIMARY KEY (id),
    CONSTRAINT fk_data_exports_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_data_exports_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT chk_data_exports_status
        CHECK (status IN ('queued', 'running', 'ready', 'failed', 'expired')),
    CONSTRAINT chk_data_exports_storage_path_len CHECK (length(storage_path) <= 512),
    -- A ready export has its blob key and retention deadline; otherwise the
    -- caller could neither download nor expire it.
    CONSTRAINT chk_data_exports_ready
        CHECK (status <> 'ready' OR (storage_path IS NOT NULL AND expires_at IS NOT NULL))
);

-- The caller's export list and one in-flight export per membership.
CREATE INDEX idx_data_exports_tenant_user_created_at_id
    ON data_exports (tenant_id, user_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX idx_data_exports_active
    ON data_exports (tenant_id, user_id)
    WHERE status IN ('queued', 'running');
-- The hourly retention purge scans every tenant by expiry.
CREATE INDEX idx_data_exports_expires_at
    ON data_exports (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE TRIGGER trigger_data_exports_updated_at
    BEFORE UPDATE ON data_exports
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: invitations (hashed single-use token)
--
-- One pending invitation per (tenant, address); re-inviting refreshes it.
-- Revoking deletes the pending row (the audit trail is audit_events);
-- accepting stamps accepted_at/accepted_by in the same transaction that
-- creates or reactivates the membership. role_id names the tenant role the
-- invitation grants, validated by the trigger below.
-- ---------------------------------------------------------------------------
CREATE TABLE invitations (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    email_normalized TEXT NOT NULL,
    role_id UUID NOT NULL,
    token_hash BYTEA NOT NULL,
    invited_by UUID NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    accepted_at TIMESTAMP WITH TIME ZONE,
    accepted_by UUID,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_invitations PRIMARY KEY (id),
    CONSTRAINT fk_invitations_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id) ON DELETE RESTRICT,
    CONSTRAINT fk_invitations_role FOREIGN KEY (role_id)
        REFERENCES roles (id) ON DELETE RESTRICT,
    CONSTRAINT fk_invitations_inviter FOREIGN KEY (tenant_id, invited_by)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_invitations_acceptor FOREIGN KEY (tenant_id, accepted_by)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT uq_invitations_token_hash UNIQUE (token_hash),
    CONSTRAINT chk_invitations_email_len CHECK (length(email_normalized) <= 255),
    CONSTRAINT chk_invitations_email_normalized
        CHECK (email_normalized = lower(btrim(email_normalized))),
    CONSTRAINT chk_invitations_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT chk_invitations_accepted CHECK ((accepted_at IS NULL) = (accepted_by IS NULL)),
    CONSTRAINT chk_invitations_expiry CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX idx_invitations_tenant_email_pending
    ON invitations (tenant_id, email_normalized)
    WHERE accepted_at IS NULL;
CREATE INDEX idx_invitations_tenant_created_at_id
    ON invitations (tenant_id, created_at DESC, id DESC);
-- The FK checks on the two membership references.
CREATE INDEX idx_invitations_tenant_invited_by
    ON invitations (tenant_id, invited_by);
CREATE INDEX idx_invitations_tenant_accepted_by
    ON invitations (tenant_id, accepted_by)
    WHERE accepted_by IS NOT NULL;
-- The expired-invitation purge.
CREATE INDEX idx_invitations_expires_at_pending
    ON invitations (expires_at)
    WHERE accepted_at IS NULL;

CREATE TRIGGER trigger_invitations_updated_at
    BEFORE UPDATE ON invitations
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- An invitation may only grant a tenant role that is either a system role or
-- belongs to the invitation's tenant. The same scope rule as membership_roles.
CREATE OR REPLACE FUNCTION validate_invitation_role()
RETURNS TRIGGER AS $$
DECLARE
    role_row roles%ROWTYPE;
BEGIN
    SELECT * INTO role_row FROM roles WHERE id = NEW.role_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'role does not exist';
    END IF;
    IF role_row.scope <> 'tenant' THEN
        RAISE EXCEPTION 'invitation roles must be tenant-scoped';
    END IF;
    IF role_row.tenant_id IS NOT NULL AND role_row.tenant_id <> NEW.tenant_id THEN
        RAISE EXCEPTION 'role belongs to another tenant';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_invitations_role_scope
    BEFORE INSERT OR UPDATE ON invitations
    FOR EACH ROW
    EXECUTE FUNCTION validate_invitation_role();

-- ---------------------------------------------------------------------------
-- identity: sessions (one per sign-in; the refresh-token family)
--
-- A session is the RFC 9700 token family: every rotated refresh token
-- references it, and presenting an already-used token revokes it
-- (revoked_reason = 'refresh_reuse'). token_version is the user's version
-- when the session was created; a later credential event makes it stale.
-- ---------------------------------------------------------------------------
CREATE TABLE sessions (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    token_version INTEGER NOT NULL,
    -- Absolute session lifetime (SESSION_MAX_LIFETIME). Refresh tokens never
    -- outlive it, and the validator rejects the session past this instant.
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    user_agent TEXT,
    ip INET,
    -- Authentication methods used to establish the session (RFC 8176). A
    -- refreshed access token repeats them; recovery codes record 'rc';
    -- passkey assertions record 'hwk' (hardware-bound) or 'swk'
    -- (software-bound).
    amr TEXT[] NOT NULL DEFAULT ARRAY['pwd']::TEXT[],
    -- When the session's second factor was last verified (POST /auth/mfa/verify
    -- or the step-up route). NULL for a first-factor-only session. AAL2
    -- gates (authzhttp.RequireMFA) require this to be within MFA_MAX_AGE.
    mfa_verified_at TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMP WITH TIME ZONE,
    revoked_reason TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_sessions PRIMARY KEY (id),
    CONSTRAINT fk_sessions_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_sessions_membership FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE CASCADE,
    -- Parent key for refresh_tokens (tenant_id, session_id).
    CONSTRAINT uq_sessions_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT chk_sessions_user_agent_len CHECK (length(user_agent) <= 512),
    CONSTRAINT chk_sessions_amr
        CHECK (amr <@ ARRAY['pwd', 'google', 'otp', 'rc', 'hwk', 'swk']::TEXT[]),
    CONSTRAINT chk_sessions_revoked CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    CONSTRAINT chk_sessions_revoked_reason
        CHECK (revoked_reason IN ('logout', 'logout_all', 'revoked', 'refresh_reuse',
                                  'credentials_changed', 'membership_changed', 'account_deleted',
                                  'mfa_changed'))
);

-- "My sessions" in the request tenant and per-tenant revocation.
CREATE INDEX idx_sessions_tenant_user_created_at
    ON sessions (tenant_id, user_id, created_at DESC);
-- Revoking every live session of a user across tenants (logout-all,
-- credential changes, account deletion).
CREATE INDEX idx_sessions_user_live
    ON sessions (user_id)
    WHERE revoked_at IS NULL;
-- The session purge (revoked, expired, or idle sessions without live refresh
-- tokens).
CREATE INDEX idx_sessions_last_used_at
    ON sessions (last_used_at);
CREATE INDEX idx_sessions_expires_at
    ON sessions (expires_at);
-- The stale-revocation purge scans revoked sessions only.
CREATE INDEX idx_sessions_revoked_at
    ON sessions (revoked_at)
    WHERE revoked_at IS NOT NULL;

CREATE TRIGGER trigger_sessions_updated_at
    BEFORE UPDATE ON sessions
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: refresh tokens (SHA-256 digests, never plaintext)
--
-- Append-only apart from used_at, which is the token's own timestamp, so the
-- table has no updated_at. Used tokens are kept until they expire so a replay
-- is recognized as reuse instead of an unknown token.
-- ---------------------------------------------------------------------------
CREATE TABLE refresh_tokens (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    session_id UUID NOT NULL,
    token_hash BYTEA NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_refresh_tokens PRIMARY KEY (id),
    CONSTRAINT fk_refresh_tokens_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_refresh_tokens_session FOREIGN KEY (tenant_id, session_id)
        REFERENCES sessions (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT uq_refresh_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT chk_refresh_tokens_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT chk_refresh_tokens_expiry CHECK (expires_at > created_at)
);

-- The FK cascade and the session purge's "no live token" check.
CREATE INDEX idx_refresh_tokens_tenant_session_id
    ON refresh_tokens (tenant_id, session_id);
-- The scheduled cross-tenant purge deletes expired rows in bounded batches.
CREATE INDEX idx_refresh_tokens_expires_at
    ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- identity: password-reset tokens (one live token per user)
--
-- Per user, not per tenant: the password is a global credential. The emailed
-- link may still carry ?tenant=<slug> so the frontend knows where to sign in
-- afterwards; the API ignores it for the lookup.
-- ---------------------------------------------------------------------------
CREATE TABLE password_reset_tokens (
    id UUID DEFAULT uuidv7(),
    user_id UUID NOT NULL,
    token_hash BYTEA NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_password_reset_tokens PRIMARY KEY (id),
    CONSTRAINT fk_password_reset_tokens_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_password_reset_tokens_user UNIQUE (user_id),
    CONSTRAINT uq_password_reset_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT chk_password_reset_tokens_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT chk_password_reset_tokens_expiry CHECK (expires_at > created_at)
);

-- ---------------------------------------------------------------------------
-- identity: email-verification tokens (bound to the target address)
-- ---------------------------------------------------------------------------
CREATE TABLE email_verification_tokens (
    id UUID DEFAULT uuidv7(),
    user_id UUID NOT NULL,
    email TEXT NOT NULL,
    token_hash BYTEA NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_email_verification_tokens PRIMARY KEY (id),
    CONSTRAINT fk_email_verification_tokens_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_email_verification_tokens_user UNIQUE (user_id),
    CONSTRAINT uq_email_verification_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT chk_email_verification_tokens_email_len CHECK (length(email) <= 255),
    CONSTRAINT chk_email_verification_tokens_token_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT chk_email_verification_tokens_expiry CHECK (expires_at > created_at)
);

-- ---------------------------------------------------------------------------
-- identity: MFA factors
--
-- MFA is per global user, not per tenant. The TOTP secret is sealed with the
-- data encryption keyring (DATA_ENCRYPTION_KEY) and never returned again after
-- enrollment. last_used_step is the RFC 6238 step of the last accepted code;
-- the verifier updates it with last_used_step < step so one code cannot be
-- replayed, including by concurrent requests.
-- ---------------------------------------------------------------------------
CREATE TABLE mfa_factors (
    id UUID DEFAULT uuidv7(),
    user_id UUID NOT NULL,
    type TEXT NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    confirmed_at TIMESTAMP WITH TIME ZONE,
    last_used_step BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_mfa_factors PRIMARY KEY (id),
    CONSTRAINT fk_mfa_factors_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT chk_mfa_factors_type_len CHECK (length(type) <= 20),
    CONSTRAINT chk_mfa_factors_type CHECK (type IN ('totp'))
);

-- At most one confirmed factor of each type per user, and one pending
-- enrollment (upsert target for re-enrollment).
CREATE UNIQUE INDEX idx_mfa_factors_user_confirmed_totp
    ON mfa_factors (user_id)
    WHERE type = 'totp' AND confirmed_at IS NOT NULL;
CREATE UNIQUE INDEX idx_mfa_factors_user_unconfirmed_totp
    ON mfa_factors (user_id)
    WHERE type = 'totp' AND confirmed_at IS NULL;

CREATE TRIGGER trigger_mfa_factors_updated_at
    BEFORE UPDATE ON mfa_factors
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: single-use recovery codes (SHA-256 digests, never plaintext)
--
-- Ten codes are issued on confirmation and on regenerate; used_at marks the
-- one that was consumed, so a code can never authenticate twice.
-- ---------------------------------------------------------------------------
CREATE TABLE mfa_recovery_codes (
    id UUID DEFAULT uuidv7(),
    user_id UUID NOT NULL,
    code_hash BYTEA NOT NULL,
    used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_mfa_recovery_codes PRIMARY KEY (id),
    CONSTRAINT fk_mfa_recovery_codes_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_mfa_recovery_codes_user_hash UNIQUE (user_id, code_hash),
    CONSTRAINT chk_mfa_recovery_codes_hash_len CHECK (octet_length(code_hash) = 32)
);

CREATE INDEX idx_mfa_recovery_codes_user_unused
    ON mfa_recovery_codes (user_id)
    WHERE used_at IS NULL;

CREATE TRIGGER trigger_mfa_recovery_codes_updated_at
    BEFORE UPDATE ON mfa_recovery_codes
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- ---------------------------------------------------------------------------
-- identity: WebAuthn credentials (passkeys)
--
-- Passkeys are per global user, like TOTP: a passkey signs in to every tenant
-- the user belongs to, so the row has no tenant_id and no RLS. The raw
-- credential public key is stored as the authenticator supplied it; the
-- private key never leaves the authenticator. credential_id is the WebAuthn
-- credential id (globally unique, base64url-decoded) and is the lookup key for
-- an assertion. sign_count is the last accepted signature counter and must
-- increase on every assertion (a regression is treated as a cloned
-- authenticator). transports mirrors the credential's transport hints.
-- ---------------------------------------------------------------------------
CREATE TABLE webauthn_credentials (
    id UUID DEFAULT uuidv7(),
    user_id UUID NOT NULL,
    credential_id BYTEA NOT NULL,
    public_key BYTEA NOT NULL,
    sign_count BIGINT NOT NULL DEFAULT 0,
    aaguid UUID,
    transports TEXT[] NOT NULL DEFAULT '{}',
    backup_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    backup_state BOOLEAN NOT NULL DEFAULT FALSE,
    name TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMP WITH TIME ZONE,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webauthn_credentials PRIMARY KEY (id),
    CONSTRAINT fk_webauthn_credentials_user FOREIGN KEY (user_id)
        REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT uq_webauthn_credentials_credential_id UNIQUE (credential_id),
    CONSTRAINT chk_webauthn_credentials_credential_id_len
        CHECK (octet_length(credential_id) BETWEEN 16 AND 1023),
    CONSTRAINT chk_webauthn_credentials_public_key_len
        CHECK (octet_length(public_key) > 0),
    CONSTRAINT chk_webauthn_credentials_sign_count
        CHECK (sign_count BETWEEN 0 AND 4294967295),
    CONSTRAINT chk_webauthn_credentials_name_len
        CHECK (length(btrim(name)) BETWEEN 1 AND 64),
    CONSTRAINT chk_webauthn_credentials_transports
        CHECK (transports <@ ARRAY['usb', 'nfc', 'ble', 'internal', 'hybrid', 'smart-card']::TEXT[]),
    CONSTRAINT chk_webauthn_credentials_backup
        CHECK (backup_eligible OR NOT backup_state)
);

-- The caller's passkey list, newest first. credential_id is globally unique
-- and is the assertion lookup path (uq_webauthn_credentials_credential_id).
CREATE INDEX idx_webauthn_credentials_user_created
    ON webauthn_credentials (user_id, created_at DESC, id DESC);

CREATE TRIGGER trigger_webauthn_credentials_updated_at
    BEFORE UPDATE ON webauthn_credentials
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- Tenant isolation. FORCE subjects the table owner too, so the app
-- role cannot silently bypass a policy it owns.
ALTER TABLE tenant_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenant_settings
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE membership_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE membership_roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON membership_roles
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
-- System roles (tenant_id NULL) are readable by every tenant; writing one
-- needs the bypass (startup catalog sync), as does any other tenant's role.
ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON roles
    USING (tenant_id IS NULL OR app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE data_exports ENABLE ROW LEVEL SECURITY;
ALTER TABLE data_exports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON data_exports
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON invitations
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sessions
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE refresh_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE refresh_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON refresh_tokens
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

-- owner: platform
-- ---------------------------------------------------------------------------
-- platform: durable jobs / outbox
-- ---------------------------------------------------------------------------
CREATE TABLE jobs (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    kind TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- W3C traceparent captured when the job was enqueued. The worker uses it
    -- for a span link only; it never affects tenant scoping or retries.
    trace_parent TEXT,
    status TEXT NOT NULL DEFAULT 'queued',
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
    CONSTRAINT pk_jobs PRIMARY KEY (id),
    CONSTRAINT fk_jobs_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    -- 'failed' is deliberately absent: a failed attempt retries (queued) or
    -- dead-letters (dead); no code writes a terminal 'failed' row.
    CONSTRAINT chk_jobs_status CHECK (status IN ('queued', 'running', 'succeeded', 'dead')),
    CONSTRAINT chk_jobs_max_attempts CHECK (max_attempts > 0),
    CONSTRAINT chk_jobs_attempts CHECK (attempts >= 0 AND attempts <= max_attempts),
    -- A running row without a deadline could never be reclaimed after its
    -- worker crashed, so the database refuses it.
    CONSTRAINT chk_jobs_running_has_lease
        CHECK (status <> 'running' OR lease_expires_at IS NOT NULL),
    -- Exactly the running rows have an owner: the claim sets locked_by and
    -- every transition out of running (succeed, fail, release, lease-expiry
    -- requeue) clears it, so ownership fences can never match a queued or
    -- terminal row.
    CONSTRAINT chk_jobs_running_has_owner
        CHECK ((status = 'running') = (locked_by IS NOT NULL))
);

-- claimJobsSQL is a cross-tenant pass: the due CTE (status = 'queued',
-- run_at <= now()) uses this. Claim and purge are global by design, so there
-- is no tenant-leading status/kind index; the only tenant-scoped lookup is the
-- idempotency key.
CREATE INDEX idx_jobs_claim
    ON jobs (run_at, id)
    WHERE status = 'queued';
-- The lease-expiry CTE scans running rows by deadline
-- (status = 'running' AND lease_expires_at < now()); the partial predicate
-- keeps the index as small as the in-flight set.
CREATE INDEX idx_jobs_running_lease_expires_at
    ON jobs (lease_expires_at)
    WHERE status = 'running';
-- Keys are scoped by tenant because callers may use user-provided keys.
CREATE UNIQUE INDEX idx_jobs_tenant_idempotency_key
    ON jobs (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- Supports the batched retention purge, which orders terminal rows by age.
-- The predicate matches purgeTerminalJobsSQL so the planner can use it.
CREATE INDEX idx_jobs_terminal_updated_at
    ON jobs (updated_at)
    WHERE status IN ('succeeded', 'dead');

-- Global claim/lease recovery is a deliberate cross-tenant job path, so those
-- statements run on the dedicated BYPASSRLS role pool.
ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON jobs
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

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
CREATE TABLE scheduler_leases (
    name TEXT NOT NULL,
    holder TEXT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    heartbeat_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_scheduler_leases PRIMARY KEY (name)
);

CREATE TRIGGER trigger_scheduler_leases_updated_at
    BEFORE UPDATE ON scheduler_leases
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE scheduler_runs (
    id UUID DEFAULT uuidv7(),
    schedule TEXT NOT NULL,
    scheduled_for TIMESTAMP WITH TIME ZONE NOT NULL,
    status TEXT NOT NULL DEFAULT 'enqueued',
    job_id UUID,
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_scheduler_runs PRIMARY KEY (id),
    CONSTRAINT chk_scheduler_runs_status CHECK (status IN ('enqueued', 'skipped', 'failed'))
);

CREATE UNIQUE INDEX idx_scheduler_runs_schedule_slot
    ON scheduler_runs (schedule, scheduled_for);
CREATE INDEX idx_scheduler_runs_created_at
    ON scheduler_runs (created_at);

-- owner: platform
-- ---------------------------------------------------------------------------
-- platform: audit trail
--
-- The trail is partitioned monthly BY RANGE (created_at): the primary key is
-- (id, created_at), writes land in the current month's partition, and
-- investigations stay tenant-scoped. A DEFAULT partition catches rows without
-- a matching month; cmd/migrate creates the current and next month's
-- partitions and documents the retention path (detach an old partition, then
-- drop it) without ever DELETEing audit rows.
--
-- parent_id is the intent row a domain event belongs to. It deliberately has
-- no foreign key: a partition-local FK is impossible and audit rows are never
-- deleted, so the reference is recorded, not enforced.
-- ---------------------------------------------------------------------------
CREATE TABLE audit_events (
    id UUID NOT NULL DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    parent_id UUID,
    actor_id UUID,
    -- What kind of principal acted. The principal's role (user/admin/...) is
    -- actor_role; it is not repeated here.
    actor_type TEXT NOT NULL DEFAULT 'unknown',
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
    source TEXT NOT NULL DEFAULT 'http',
    outcome TEXT NOT NULL DEFAULT 'succeeded',
    status_code SMALLINT,
    error_code TEXT,
    idempotency_key_hash BYTEA,
    completed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    -- A partitioned table's primary key must contain the partition key.
    CONSTRAINT pk_audit_events PRIMARY KEY (id, created_at),
    CONSTRAINT fk_audit_events_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT chk_audit_events_actor_type
        CHECK (actor_type IN ('user', 'service', 'job', 'system', 'anonymous', 'unknown')),
    CONSTRAINT chk_audit_events_source
        CHECK (source IN ('http', 'service', 'job', 'system')),
    CONSTRAINT chk_audit_events_outcome
        CHECK (outcome IN ('started', 'succeeded', 'failed', 'denied', 'unknown')),
    CONSTRAINT chk_audit_events_idempotency_key_hash_len
        CHECK (idempotency_key_hash IS NULL OR octet_length(idempotency_key_hash) = 32)
) PARTITION BY RANGE (created_at);

-- The application only inserts rows and completes them by primary key; the
-- secondary indexes back operator investigation queries, always tenant-scoped
-- (by time, actor, action, or resource), plus request-id correlation, parent
-- (intent -> domain event) lookups, and the stale-intent check in
-- docs/operations.md (outcome = 'started' AND created_at < ...). The global
-- actor/action/resource duplicates of the tenant-leading indexes were dropped:
-- the table is append-only and write-heavy, and investigations are per tenant.
CREATE INDEX idx_audit_events_tenant_created
    ON audit_events (tenant_id, created_at DESC);
CREATE INDEX idx_audit_events_tenant_actor_created
    ON audit_events (tenant_id, actor_id, created_at DESC);
CREATE INDEX idx_audit_events_tenant_action_created
    ON audit_events (tenant_id, action, created_at DESC);
CREATE INDEX idx_audit_events_tenant_resource
    ON audit_events (tenant_id, resource, resource_id);
CREATE INDEX idx_audit_events_request_id
    ON audit_events (request_id)
    WHERE request_id IS NOT NULL;
CREATE INDEX idx_audit_events_parent_id
    ON audit_events (parent_id)
    WHERE parent_id IS NOT NULL;
CREATE INDEX idx_audit_events_started
    ON audit_events (tenant_id, created_at)
    WHERE outcome = 'started';

-- The default partition exists immediately so an insert before cmd/migrate
-- creates the monthly partitions still succeeds. The statement is built in a
-- DO block because the partition name is derived, not a static DDL object of
-- the init migration.
DO $$
DECLARE
    target_schema TEXT := current_schema();
BEGIN
    IF target_schema IS NULL THEN
        RAISE EXCEPTION 'cannot create the audit_events default partition: current_schema() is NULL';
    END IF;
    EXECUTE format('CREATE TABLE %I.%I PARTITION OF %I.audit_events DEFAULT',
        target_schema, 'audit_events_default', target_schema);
    -- Partitions are tables of their own: a direct read of a partition skips
    -- the parent's policy, so every partition carries the same RLS.
    EXECUTE format('ALTER TABLE %I.%I ENABLE ROW LEVEL SECURITY', target_schema, 'audit_events_default');
    EXECUTE format('ALTER TABLE %I.%I FORCE ROW LEVEL SECURITY', target_schema, 'audit_events_default');
    EXECUTE format('CREATE POLICY tenant_isolation ON %I.%I USING (app_tenant_visible(tenant_id)) WITH CHECK (app_tenant_visible(tenant_id))',
        target_schema, 'audit_events_default');
END;
$$;

-- Audit rows are append-only. Only a pre-write intent row (source http,
-- outcome started) may transition to a terminal outcome, and only completion
-- fields may change. This makes a successful mutation's audit row immutable.
-- The actor is part of the intent: a completion may not swap in a different
-- actor, and status_code/error_code may only be filled once (NULL -> value),
-- never changed afterwards.
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
        OR NEW.actor_id IS DISTINCT FROM OLD.actor_id
        OR NEW.actor_type IS DISTINCT FROM OLD.actor_type
        OR NEW.actor_role IS DISTINCT FROM OLD.actor_role
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
    -- status_code and error_code are completion fields, not intent fields:
    -- the first non-NULL value is set once and can never change.
    IF OLD.status_code IS NOT NULL AND NEW.status_code IS DISTINCT FROM OLD.status_code THEN
        RAISE EXCEPTION 'audit completion cannot change status_code';
    END IF;
    IF OLD.error_code IS NOT NULL AND NEW.error_code IS DISTINCT FROM OLD.error_code THEN
        RAISE EXCEPTION 'audit completion cannot change error_code';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_audit_events_completion
    BEFORE UPDATE ON audit_events
    FOR EACH ROW
    EXECUTE FUNCTION audit_events_completion_guard();

-- Deleting audit rows is never an application path. A reviewed retention job
-- detaches and drops an old monthly partition instead. These triggers live on
-- the partitioned parent and therefore cover every partition.
CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events rows are append-only (% is not allowed)', TG_OP;
END;
$$ LANGUAGE plpgsql;

-- Trigger functions resolve their unqualified table names through the
-- caller's search_path. An attacker who can create a TEMP table in a session
-- could otherwise shadow `roles`, `memberships`, or `tenants` for the
-- duration of one statement and subvert the checks above. Pin pg_catalog plus
-- the schema this migration targets (DB_SCHEMA; the migration runs with
-- search_path set to it, so current_schema() is that schema).
--
-- app_tenant_visible deliberately stays a plain SQL function with no SET
-- clause: it reads no table and must remain inlinable into every tenant
-- policy. database-roles.sql also revokes TEMP on the database from the
-- runtime roles, which removes the shadowing primitive itself.
DO $$
DECLARE
    target_schema TEXT := current_schema();
BEGIN
    IF target_schema IS NULL THEN
        RAISE EXCEPTION 'cannot pin function search_path: current_schema() is NULL';
    END IF;
    EXECUTE format(
        'ALTER FUNCTION %I.update_updated_at() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.validate_role_permission() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.validate_membership_role() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.validate_platform_role() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.validate_invitation_role() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.protect_last_owner() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.audit_events_completion_guard() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
    EXECUTE format(
        'ALTER FUNCTION %I.audit_events_append_only() SET search_path = pg_catalog, %I',
        target_schema, target_schema);
END;
$$;

CREATE TRIGGER trigger_audit_events_no_delete
    BEFORE DELETE ON audit_events
    FOR EACH ROW
    EXECUTE FUNCTION audit_events_append_only();

CREATE TRIGGER trigger_audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT
    EXECUTE FUNCTION audit_events_append_only();

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_events
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

-- owner: apikeys
-- ---------------------------------------------------------------------------
-- apikeys: tenant/user-bound API credentials (only SHA-256 digests at rest)
--
-- prefix_<32-byte base64url> credentials are shown once at creation/rotation.
-- A rotated key remains valid until rotation_grace_until; explicit revocation
-- always wins. Every key expires: expires_at is required and must be after
-- created_at. Scopes are rows of api_key_scopes validated by the module
-- against the authz catalog. The stored permission strings (e.g.
-- "files:read") are a persisted, stable API: renaming a permission in authz
-- requires a data migration of api_key_scopes.
-- ---------------------------------------------------------------------------
CREATE TABLE api_keys (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    -- The composite FK below makes the database reject a key whose owner
    -- is not a member of the key's tenant (parent key: uq_memberships_tenant_user).
    user_id UUID NOT NULL,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    key_hash BYTEA NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_at TIMESTAMP WITH TIME ZONE,
    rotated_at TIMESTAMP WITH TIME ZONE,
    rotation_grace_until TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_api_keys PRIMARY KEY (id),
    CONSTRAINT fk_api_keys_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_api_keys_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT chk_api_keys_name_len CHECK (length(name) <= 255),
    CONSTRAINT chk_api_keys_prefix_len CHECK (length(prefix) <= 16),
    CONSTRAINT chk_api_keys_key_hash_len CHECK (octet_length(key_hash) = 32),
    CONSTRAINT chk_api_keys_expiry CHECK (expires_at > created_at),
    -- MarkAPIKeyRotated is the only writer of either column and always sets
    -- both, so a grace window exists exactly when the key was rotated.
    CONSTRAINT chk_api_keys_rotation_grace_matches_rotated
        CHECK ((rotation_grace_until IS NULL) = (rotated_at IS NULL))
);

CREATE UNIQUE INDEX idx_api_keys_key_hash
    ON api_keys (key_hash);
CREATE INDEX idx_api_keys_tenant_user
    ON api_keys (tenant_id, user_id);
CREATE INDEX idx_api_keys_tenant_created_at_id
    ON api_keys (tenant_id, created_at DESC, id DESC);

CREATE TRIGGER trigger_api_keys_updated_at
    BEFORE UPDATE ON api_keys
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- One row per granted permission. The rows are fully owned by their key
-- (CASCADE) and carry no tenant_id: the key id is global and the parent row
-- holds the tenant. permission_key is an authz permission; the CHECK mirrors
-- authz.permissionPattern and the FK pins it to the synchronized catalog.
CREATE TABLE api_key_scopes (
    api_key_id UUID NOT NULL,
    permission_key TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_api_key_scopes PRIMARY KEY (api_key_id, permission_key),
    CONSTRAINT fk_api_key_scopes_key FOREIGN KEY (api_key_id)
        REFERENCES api_keys (id) ON DELETE CASCADE,
    -- The catalog is synchronized at API startup; removing a permission
    -- cascades to the affected key scopes (fail closed: the key loses it).
    -- role_permissions keeps RESTRICT (a role must be reviewed explicitly).
    CONSTRAINT fk_api_key_scopes_permission FOREIGN KEY (permission_key)
        REFERENCES permissions (key) ON DELETE CASCADE,
    CONSTRAINT chk_api_key_scopes_permission_key
        CHECK (permission_key ~ '^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)+$')
);

-- api_key_scopes has no tenant_id: it is a pure child of api_keys, reachable
-- only through a key RLS already filtered.
ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON api_keys
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

-- owner: files
-- ---------------------------------------------------------------------------
-- files: metadata (blobs live in local/S3 storage)
-- ---------------------------------------------------------------------------
CREATE TABLE files (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    original_name TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    mime_type TEXT NOT NULL,
    size BIGINT NOT NULL CHECK (size >= 0),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    -- files.trash.purge claim: set before the blob is deleted; restore requires NULL.
    purge_started_at TIMESTAMP WITH TIME ZONE,
    -- A blob delete failure releases the claim (restore stays possible) and
    -- records the attempt; rows are skipped until purge_retry_after so a
    -- permanently failing blob cannot block newer rows.
    purge_attempts INTEGER NOT NULL DEFAULT 0 CHECK (purge_attempts >= 0),
    purge_retry_after TIMESTAMP WITH TIME ZONE,
    CONSTRAINT pk_files PRIMARY KEY (id),
    CONSTRAINT fk_files_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_files_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT uq_files_storage_path UNIQUE (storage_path),
    CONSTRAINT chk_files_original_name_len CHECK (length(original_name) <= 255),
    CONSTRAINT chk_files_storage_path_len CHECK (length(storage_path) <= 512),
    CONSTRAINT chk_files_mime_type_len CHECK (length(mime_type) <= 127)
);

-- Deliberately NOT partial: it serves ListFilesByUserCursor for both
-- include_deleted values under a generic plan, ListFilesByUserID and
-- CountFilesByUserID (deleted_at is a cheap heap filter), and the
-- fk_files_tenant_user check when a membership row is deleted or re-keyed.
CREATE INDEX idx_files_tenant_user_created
    ON files (tenant_id, user_id, created_at DESC, id DESC);
-- AdminListFilesCursor / AdminListFiles / AdminFileStats.
CREATE INDEX idx_files_tenant_created_at_id
    ON files (tenant_id, created_at DESC, id DESC);
-- files.trash.purge scans every tenant by age, so this leads with deleted_at.
CREATE INDEX idx_files_deleted_at
    ON files (deleted_at)
    WHERE deleted_at IS NOT NULL;

CREATE TRIGGER trigger_files_updated_at
    BEFORE UPDATE ON files
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

ALTER TABLE files ENABLE ROW LEVEL SECURITY;
ALTER TABLE files FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON files
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

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
CREATE TABLE feature_flags (
    flag_key TEXT NOT NULL,
    enabled BOOLEAN,
    kill_switch BOOLEAN NOT NULL DEFAULT FALSE,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_feature_flags PRIMARY KEY (flag_key),
    CONSTRAINT chk_feature_flags_flag_key_len CHECK (length(flag_key) <= 128),
    CONSTRAINT chk_feature_flags_description_len CHECK (length(description) <= 500)
);

CREATE TRIGGER trigger_feature_flags_updated_at
    BEFORE UPDATE ON feature_flags
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE feature_flag_targets (
    id UUID DEFAULT uuidv7(),
    flag_key TEXT NOT NULL,
    tenant_id UUID NOT NULL,
    user_id UUID,
    enabled BOOLEAN NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_feature_flag_targets PRIMARY KEY (id),
    CONSTRAINT fk_feature_flag_targets_flag_key FOREIGN KEY (flag_key)
        REFERENCES feature_flags (flag_key) ON DELETE RESTRICT,
    CONSTRAINT fk_feature_flag_targets_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_feature_flag_targets_user_tenant FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT chk_feature_flag_targets_flag_key_len CHECK (length(flag_key) <= 128)
);

-- One tenant-wide target (user_id IS NULL) and one target per user per flag:
-- NULLS NOT DISTINCT makes the NULL tenant-wide target collide with itself,
-- so a single constraint (and a single ON CONFLICT target) covers both kinds.
-- It is also the tenant-leading index of the admin list query.
CREATE UNIQUE INDEX idx_feature_flag_targets_tenant_flag_user
    ON feature_flag_targets (tenant_id, flag_key, user_id) NULLS NOT DISTINCT;
-- Backs the fk_feature_flag_targets_flag_key RESTRICT check that runs on
-- every DeleteFeatureFlag; the unique index above leads with tenant_id.
CREATE INDEX idx_feature_flag_targets_flag_key
    ON feature_flag_targets (flag_key);

CREATE TRIGGER trigger_feature_flag_targets_updated_at
    BEFORE UPDATE ON feature_flag_targets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- feature_flags stays unprotected: it is process-wide configuration. Loads of
-- the full target snapshot run on the dedicated BYPASSRLS role pool.
ALTER TABLE feature_flag_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE feature_flag_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON feature_flag_targets
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

-- owner: notifications
-- ---------------------------------------------------------------------------
-- notifications: per-user in-app feed and channel preferences
--
-- A notification row is the in-app delivery. Email delivery is a durable job
-- enqueued in the same transaction as the row, so a queue failure cannot leave
-- a half-delivered notification behind. Users may disable either channel per
-- type; a missing preference row means both channels are enabled.
-- ---------------------------------------------------------------------------
CREATE TABLE notifications (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    type TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    data JSONB NOT NULL DEFAULT '{}'::jsonb,
    read_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_notifications PRIMARY KEY (id),
    CONSTRAINT fk_notifications_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_notifications_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT chk_notifications_type_len CHECK (length(type) <= 100),
    CONSTRAINT chk_notifications_title_len CHECK (length(title) <= 255)
);

-- The non-partial index also serves the fk_notifications_tenant_user check.
CREATE INDEX idx_notifications_tenant_user_created_at_id
    ON notifications (tenant_id, user_id, created_at DESC, id DESC);
CREATE INDEX idx_notifications_tenant_user_unread
    ON notifications (tenant_id, user_id, created_at DESC, id DESC)
    WHERE read_at IS NULL;
-- notifications.retention.purge scans every tenant by read_at.
CREATE INDEX idx_notifications_read_at
    ON notifications (read_at)
    WHERE read_at IS NOT NULL;

CREATE TRIGGER trigger_notifications_updated_at
    BEFORE UPDATE ON notifications
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- The natural key is the row's identity: nothing addresses a preference by a
-- surrogate id, so (tenant, user, type, channel) is the primary key and the
-- ON CONFLICT target. The PK also serves the membership FK check.
CREATE TABLE notification_preferences (
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    type TEXT NOT NULL,
    channel TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_notification_preferences PRIMARY KEY (tenant_id, user_id, type, channel),
    CONSTRAINT fk_notification_preferences_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_notification_preferences_tenant_user FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT chk_notification_preferences_type_len CHECK (length(type) <= 100),
    CONSTRAINT chk_notification_preferences_channel_len CHECK (length(channel) <= 20),
    CONSTRAINT chk_notification_preferences_channel CHECK (channel IN ('in_app', 'email'))
);

CREATE TRIGGER trigger_notification_preferences_updated_at
    BEFORE UPDATE ON notification_preferences
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notifications
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE notification_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_preferences FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_preferences
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));

-- owner: webhooks
-- ---------------------------------------------------------------------------
-- webhooks: outbound endpoints, their event subscriptions, and versioned
-- signing secrets
--
-- Secrets are sealed with AES-256-GCM by the webhooks module and only the
-- ciphertext is stored. Rotating a secret deactivates the old row and inserts a
-- new version; the retired row keeps an explicit 24h grace expiry so deliveries
-- can be signed with both the previous and the current secret while receivers
-- switch over. A second rotation expires the older retired row: only the
-- immediately previous secret is ever valid.
-- ---------------------------------------------------------------------------
CREATE TABLE webhook_endpoints (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    url TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'active',
    max_attempts INTEGER NOT NULL DEFAULT 8,
    timeout_ms INTEGER NOT NULL DEFAULT 10000,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    CONSTRAINT pk_webhook_endpoints PRIMARY KEY (id),
    CONSTRAINT fk_webhook_endpoints_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT chk_webhook_endpoints_url_len CHECK (length(url) <= 2048),
    CONSTRAINT chk_webhook_endpoints_description_len CHECK (length(description) <= 255),
    CONSTRAINT chk_webhook_endpoints_status CHECK (status IN ('active', 'disabled')),
    CONSTRAINT chk_webhook_endpoints_max_attempts CHECK (max_attempts BETWEEN 1 AND 50),
    CONSTRAINT chk_webhook_endpoints_timeout_ms CHECK (timeout_ms BETWEEN 100 AND 120000)
);

-- Parent key for the tenant-composite FKs from subscriptions, secrets, and
-- deliveries.
CREATE UNIQUE INDEX idx_webhook_endpoints_tenant_id_id
    ON webhook_endpoints (tenant_id, id);
CREATE INDEX idx_webhook_endpoints_tenant_created_at_id
    ON webhook_endpoints (tenant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_webhook_endpoints_tenant_status
    ON webhook_endpoints (tenant_id, status)
    WHERE deleted_at IS NULL;

CREATE TRIGGER trigger_webhook_endpoints_updated_at
    BEFORE UPDATE ON webhook_endpoints
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- One row per event-type pattern an endpoint subscribes to: an exact type
-- ("file.uploaded"), a prefix wildcard ("file.*"), or "*". Matching happens in
-- the dispatcher (webhookcontracts.Match); the patterns are validated there
-- too. Subscriptions are configuration fully owned by their endpoint, so
-- CASCADE; endpoints are soft-deleted, so this only fires on a hard delete.
CREATE TABLE webhook_endpoint_subscriptions (
    tenant_id UUID NOT NULL,
    endpoint_id UUID NOT NULL,
    pattern TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_endpoint_subscriptions PRIMARY KEY (endpoint_id, pattern),
    CONSTRAINT fk_webhook_endpoint_subscriptions_tenant_endpoint FOREIGN KEY (tenant_id, endpoint_id)
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_webhook_endpoint_subscriptions_pattern_len CHECK (length(pattern) <= 128),
    CONSTRAINT chk_webhook_endpoint_subscriptions_pattern CHECK (pattern <> '')
);

CREATE TABLE webhook_endpoint_secrets (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    endpoint_id UUID NOT NULL,
    version INTEGER NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    -- Set when the row is retired: the secret keeps co-signing deliveries
    -- until then. NULL exactly on the active row; a retired row always has an
    -- expiry (a second rotation moves it to now()).
    grace_expires_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_endpoint_secrets PRIMARY KEY (id),
    -- Secrets are fully owned by their endpoint (same module), so CASCADE.
    CONSTRAINT fk_webhook_endpoint_secrets_tenant_endpoint FOREIGN KEY (tenant_id, endpoint_id)
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_webhook_endpoint_secrets_version CHECK (version > 0),
    CONSTRAINT chk_webhook_endpoint_secrets_grace
        CHECK (active = (grace_expires_at IS NULL))
);

CREATE UNIQUE INDEX idx_webhook_endpoint_secrets_endpoint_version
    ON webhook_endpoint_secrets (endpoint_id, version);
CREATE UNIQUE INDEX idx_webhook_endpoint_secrets_active
    ON webhook_endpoint_secrets (tenant_id, endpoint_id)
    WHERE active;
CREATE INDEX idx_webhook_endpoint_secrets_tenant_endpoint
    ON webhook_endpoint_secrets (tenant_id, endpoint_id);
-- Parent key for the tenant-composite FK from webhook_deliveries.secret_id.
CREATE UNIQUE INDEX idx_webhook_endpoint_secrets_tenant_id_id
    ON webhook_endpoint_secrets (tenant_id, id);

CREATE TRIGGER trigger_webhook_endpoint_secrets_updated_at
    BEFORE UPDATE ON webhook_endpoint_secrets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

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
-- A delivery's url/timeout_ms/max_attempts are snapshots taken from the
-- endpoint at dispatch: editing an endpoint cannot change a queued delivery's
-- signed request. secret_id references the exact secret version (a composite
-- tenant FK): a rotation never changes a queued delivery's signing key, and
-- the ciphertext is not duplicated per delivery. event_id is deduplicated per
-- endpoint so re-dispatching the same domain event is safe.
-- ---------------------------------------------------------------------------
-- The columns are the versioned event envelope: event_id (UUIDv7),
-- event_type, data_version, occurred_at, tenant_id, actor_type/actor_id, and
-- payload (the envelope's data). The delivery worker rebuilds the signed
-- JSON body from them.
CREATE TABLE webhook_events (
    tenant_id UUID NOT NULL,
    event_id UUID NOT NULL,
    event_type TEXT NOT NULL,
    data_version INTEGER NOT NULL,
    actor_type TEXT NOT NULL,
    -- A system actor has no id; user and service actors always have one.
    actor_id UUID,
    occurred_at TIMESTAMP WITH TIME ZONE NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_events PRIMARY KEY (tenant_id, event_id),
    CONSTRAINT fk_webhook_events_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT chk_webhook_events_event_type_len CHECK (length(event_type) <= 128),
    CONSTRAINT chk_webhook_events_data_version CHECK (data_version > 0),
    CONSTRAINT chk_webhook_events_actor_type
        CHECK (actor_type IN ('user', 'service', 'system')),
    CONSTRAINT chk_webhook_events_actor
        CHECK ((actor_type = 'system') = (actor_id IS NULL))
);

-- Cross-tenant retention pass (PurgeOrphanWebhookEvents): oldest events first.
CREATE INDEX idx_webhook_events_created_at
    ON webhook_events (created_at);

CREATE TABLE webhook_deliveries (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    -- Deliveries are history: endpoints are soft-deleted and a hard delete
    -- must not silently erase the delivery log (RESTRICT, tenant-composite).
    endpoint_id UUID NOT NULL,
    -- RESTRICT: an event is purged only after every delivery of it is.
    event_id UUID NOT NULL,
    -- RESTRICT: the signing secret a queued delivery snapshots must outlive
    -- the delivery; secrets are only removed with their endpoint.
    secret_id UUID NOT NULL,
    url TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL,
    -- Per-request timeout snapshotted from the endpoint, like url.
    timeout_ms INTEGER NOT NULL DEFAULT 10000,
    next_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_status_code INTEGER,
    last_error TEXT,
    delivered_at TIMESTAMP WITH TIME ZONE,
    -- Single-column on purpose: a composite FK with ON DELETE SET NULL would
    -- also null tenant_id when the original delivery is purged.
    replayed_from_id UUID,
    -- is_replay is immutable and keeps replay rows out of the dispatch
    -- deduplication index, so a replay can target the same event and endpoint
    -- again while webhook-id stays the original message id.
    is_replay BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_deliveries PRIMARY KEY (id),
    CONSTRAINT fk_webhook_deliveries_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT fk_webhook_deliveries_tenant_endpoint FOREIGN KEY (tenant_id, endpoint_id)
        REFERENCES webhook_endpoints (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_webhook_deliveries_tenant_event FOREIGN KEY (tenant_id, event_id)
        REFERENCES webhook_events (tenant_id, event_id) ON DELETE RESTRICT,
    CONSTRAINT fk_webhook_deliveries_tenant_secret FOREIGN KEY (tenant_id, secret_id)
        REFERENCES webhook_endpoint_secrets (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_webhook_deliveries_replayed_from FOREIGN KEY (replayed_from_id)
        REFERENCES webhook_deliveries (id) ON DELETE SET NULL,
    CONSTRAINT chk_webhook_deliveries_url_len CHECK (length(url) <= 2048),
    CONSTRAINT chk_webhook_deliveries_status CHECK (status IN ('pending', 'succeeded', 'dead')),
    CONSTRAINT chk_webhook_deliveries_attempts CHECK (attempts >= 0 AND attempts <= max_attempts),
    CONSTRAINT chk_webhook_deliveries_max_attempts CHECK (max_attempts BETWEEN 1 AND 50),
    CONSTRAINT chk_webhook_deliveries_timeout_ms CHECK (timeout_ms BETWEEN 100 AND 120000),
    -- A succeeded delivery has a delivery timestamp; no other state does.
    CONSTRAINT chk_webhook_deliveries_succeeded
        CHECK ((status = 'succeeded') = (delivered_at IS NOT NULL))
);

-- Parent key for the tenant-composite FK from webhook_delivery_attempts.
CREATE UNIQUE INDEX idx_webhook_deliveries_tenant_id_id
    ON webhook_deliveries (tenant_id, id);
-- Dispatch deduplication only: replays are new delivery rows for the same
-- message, so they are excluded by the immutable is_replay flag (replayed_from_id
-- is mutable through ON DELETE SET NULL and cannot carry the predicate).
CREATE UNIQUE INDEX idx_webhook_deliveries_endpoint_event
    ON webhook_deliveries (tenant_id, endpoint_id, event_id)
    WHERE NOT is_replay;
-- Serves the fk_webhook_deliveries_tenant_event check when an event is
-- deleted and the NOT EXISTS probe of PurgeOrphanWebhookEvents.
CREATE INDEX idx_webhook_deliveries_tenant_event
    ON webhook_deliveries (tenant_id, event_id);
CREATE INDEX idx_webhook_deliveries_tenant_created_at_id
    ON webhook_deliveries (tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_webhook_deliveries_tenant_endpoint_created
    ON webhook_deliveries (tenant_id, endpoint_id, created_at DESC, id DESC);
-- Serves the fk_webhook_deliveries_tenant_secret check when a secret row is
-- removed with its endpoint.
CREATE INDEX idx_webhook_deliveries_tenant_secret
    ON webhook_deliveries (tenant_id, secret_id);
-- Retention is a cross-tenant maintenance pass, so these purge indexes are
-- deliberately not tenant-leading. The predicate matches PurgeWebhookDeliveries.
CREATE INDEX idx_webhook_deliveries_terminal_updated_at
    ON webhook_deliveries (updated_at)
    WHERE status IN ('succeeded', 'dead');
-- Purging an original delivery runs ON DELETE SET NULL against its replays;
-- without this index every deleted row would scan webhook_deliveries.
CREATE INDEX idx_webhook_deliveries_replayed_from_id
    ON webhook_deliveries (replayed_from_id)
    WHERE replayed_from_id IS NOT NULL;

CREATE TRIGGER trigger_webhook_deliveries_updated_at
    BEFORE UPDATE ON webhook_deliveries
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE webhook_delivery_attempts (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    delivery_id UUID NOT NULL,
    attempt INTEGER NOT NULL,
    status_code INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_delivery_attempts PRIMARY KEY (id),
    CONSTRAINT fk_webhook_delivery_attempts_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    -- Tenant-composite so an attempt can never carry a different tenant than
    -- its delivery (parent key: idx_webhook_deliveries_tenant_id_id). CASCADE
    -- by design: attempts are fully owned by their delivery, and the batched
    -- retention purge (PurgeWebhookDeliveries) relies on it to remove a
    -- delivery's attempts in the same statement.
    CONSTRAINT fk_webhook_delivery_attempts_tenant_delivery FOREIGN KEY (tenant_id, delivery_id)
        REFERENCES webhook_deliveries (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_webhook_delivery_attempts_attempt CHECK (attempt > 0),
    CONSTRAINT chk_webhook_delivery_attempts_duration CHECK (duration_ms >= 0)
);

CREATE UNIQUE INDEX idx_webhook_delivery_attempts_delivery_attempt
    ON webhook_delivery_attempts (delivery_id, attempt);
-- Tenant-leading index for ListWebhookDeliveryAttempts; it also serves the
-- fk_webhook_delivery_attempts_tenant_delivery cascade. Attempts have no
-- time-ordered listing, so there is no (tenant_id, created_at) index.
CREATE INDEX idx_webhook_delivery_attempts_tenant_delivery
    ON webhook_delivery_attempts (tenant_id, delivery_id);

-- ---------------------------------------------------------------------------
-- webhooks: inbound sources, their versioned secrets, and deduplicated events
--
-- The public receiver authenticates HMAC over the raw body and timestamp. The
-- source secret is sealed at rest but must be decryptable for verification.
-- Secrets are versioned rows like the outbound ones: rotating deactivates the
-- old row and keeps it usable for the explicit 24h grace window; a second
-- rotation expires it, so only the immediately previous secret is accepted.
-- Inbound events deduplicate on (tenant, source, external_event_id).
-- ---------------------------------------------------------------------------
CREATE TABLE webhook_inbound_sources (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    slug TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE,
    CONSTRAINT pk_webhook_inbound_sources PRIMARY KEY (id),
    CONSTRAINT fk_webhook_inbound_sources_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    CONSTRAINT chk_webhook_inbound_sources_name_len CHECK (length(name) <= 255),
    CONSTRAINT chk_webhook_inbound_sources_slug_len CHECK (length(slug) <= 63),
    CONSTRAINT chk_webhook_inbound_sources_status CHECK (status IN ('active', 'disabled'))
);

CREATE UNIQUE INDEX idx_webhook_inbound_sources_tenant_slug_active
    ON webhook_inbound_sources (tenant_id, slug)
    WHERE deleted_at IS NULL;
-- Parent key for the tenant-composite FKs from secrets and events.
CREATE UNIQUE INDEX idx_webhook_inbound_sources_tenant_id_id
    ON webhook_inbound_sources (tenant_id, id);
CREATE INDEX idx_webhook_inbound_sources_tenant_created
    ON webhook_inbound_sources (tenant_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

CREATE TRIGGER trigger_webhook_inbound_sources_updated_at
    BEFORE UPDATE ON webhook_inbound_sources
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE webhook_inbound_source_secrets (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    source_id UUID NOT NULL,
    version INTEGER NOT NULL,
    secret_ciphertext TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    grace_expires_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_inbound_source_secrets PRIMARY KEY (id),
    CONSTRAINT fk_webhook_inbound_source_secrets_tenant_source FOREIGN KEY (tenant_id, source_id)
        REFERENCES webhook_inbound_sources (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_webhook_inbound_source_secrets_version CHECK (version > 0),
    CONSTRAINT chk_webhook_inbound_source_secrets_grace
        CHECK (active = (grace_expires_at IS NULL))
);

CREATE UNIQUE INDEX idx_webhook_inbound_source_secrets_source_version
    ON webhook_inbound_source_secrets (source_id, version);
CREATE UNIQUE INDEX idx_webhook_inbound_source_secrets_active
    ON webhook_inbound_source_secrets (tenant_id, source_id)
    WHERE active;
CREATE INDEX idx_webhook_inbound_source_secrets_tenant_source
    ON webhook_inbound_source_secrets (tenant_id, source_id);

CREATE TRIGGER trigger_webhook_inbound_source_secrets_updated_at
    BEFORE UPDATE ON webhook_inbound_source_secrets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

CREATE TABLE webhook_inbound_events (
    id UUID DEFAULT uuidv7(),
    tenant_id UUID NOT NULL,
    source_id UUID NOT NULL,
    external_event_id TEXT NOT NULL,
    event_type TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL,
    payload JSONB NOT NULL,
    received_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_webhook_inbound_events PRIMARY KEY (id),
    CONSTRAINT fk_webhook_inbound_events_tenant FOREIGN KEY (tenant_id)
        REFERENCES tenants (id),
    -- Inbound events are history and sources are soft-deleted: RESTRICT so a
    -- hard delete of a source cannot erase its events; retention purges them.
    CONSTRAINT fk_webhook_inbound_events_tenant_source FOREIGN KEY (tenant_id, source_id)
        REFERENCES webhook_inbound_sources (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT chk_webhook_inbound_events_external_id_len CHECK (length(external_event_id) <= 255),
    CONSTRAINT chk_webhook_inbound_events_event_type_len CHECK (length(event_type) <= 128),
    CONSTRAINT chk_webhook_inbound_events_signature_len CHECK (length(signature) <= 255)
);

CREATE UNIQUE INDEX idx_webhook_inbound_events_source_event
    ON webhook_inbound_events (tenant_id, source_id, external_event_id);
CREATE INDEX idx_webhook_inbound_events_tenant_source_received
    ON webhook_inbound_events (tenant_id, source_id, received_at DESC, id DESC);
-- Cross-tenant retention purge (PurgeWebhookInboundEvents).
CREATE INDEX idx_webhook_inbound_events_received_at
    ON webhook_inbound_events (received_at);

-- webhook_delivery_attempts and webhook_inbound_source_secrets are
-- tenant-owned too, so each gets the same policy.
ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_endpoints
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_endpoint_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoint_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_endpoint_subscriptions
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_endpoint_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoint_secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_endpoint_secrets
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_events
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_deliveries
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_delivery_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_delivery_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_delivery_attempts
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_inbound_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_inbound_sources FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_inbound_sources
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_inbound_source_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_inbound_source_secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_inbound_source_secrets
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
ALTER TABLE webhook_inbound_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_inbound_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhook_inbound_events
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
