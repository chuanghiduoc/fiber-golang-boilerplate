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
