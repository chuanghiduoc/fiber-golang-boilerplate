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
