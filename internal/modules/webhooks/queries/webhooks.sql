-- Outbound endpoints -------------------------------------------------------
--
-- An endpoint's event-type patterns are rows of webhook_endpoint_subscriptions.
-- Reads return them as one event_types text[] column sorted in byte order
-- (COLLATE "C", matching the service's sort.Strings).

-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints (
    tenant_id, url, description, status, max_attempts, timeout_ms
)
VALUES (
    @tenant_id, @url, @description, @status, @max_attempts, @timeout_ms
)
RETURNING *;

-- name: CreateWebhookEndpointSubscriptions :exec
INSERT INTO webhook_endpoint_subscriptions (tenant_id, endpoint_id, pattern)
SELECT @tenant_id, @endpoint_id, pattern
FROM unnest(@patterns::text[]) AS pattern;

-- name: DeleteWebhookEndpointSubscriptions :exec
DELETE FROM webhook_endpoint_subscriptions
WHERE tenant_id = @tenant_id AND endpoint_id = @endpoint_id;

-- name: GetWebhookEndpoint :one
SELECT sqlc.embed(e),
       ARRAY(SELECT s.pattern FROM webhook_endpoint_subscriptions s
             WHERE s.tenant_id = e.tenant_id AND s.endpoint_id = e.id
             ORDER BY s.pattern COLLATE "C")::text[] AS event_types
FROM webhook_endpoints e
WHERE e.id = @id AND e.tenant_id = @tenant_id AND e.deleted_at IS NULL;

-- name: ListWebhookEndpoints :many
SELECT sqlc.embed(e),
       ARRAY(SELECT s.pattern FROM webhook_endpoint_subscriptions s
             WHERE s.tenant_id = e.tenant_id AND s.endpoint_id = e.id
             ORDER BY s.pattern COLLATE "C")::text[] AS event_types
FROM webhook_endpoints e
WHERE e.tenant_id = @tenant_id
  AND e.deleted_at IS NULL
  AND (e.created_at, e.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY e.created_at DESC, e.id DESC
LIMIT @row_limit::int;

-- name: UpdateWebhookEndpoint :one
UPDATE webhook_endpoints
SET url = @url,
    description = @description,
    status = @status,
    max_attempts = @max_attempts,
    timeout_ms = @timeout_ms
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
RETURNING *;

-- name: UpdateWebhookDeliveryEndpointSnapshot :exec
-- Runs in the endpoint update transaction: a queued delivery must follow the
-- endpoint's new URL/timeout instead of retrying the old receiver. Terminal
-- (succeeded/dead) rows keep the snapshot they were sent with.
UPDATE webhook_deliveries
SET url = @url,
    timeout_ms = @timeout_ms
WHERE tenant_id = @tenant_id
  AND endpoint_id = @endpoint_id
  AND status = 'pending';

-- name: DeleteWebhookEndpoint :execrows
UPDATE webhook_endpoints
SET status = 'disabled', deleted_at = NOW()
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: ExpireOlderWebhookEndpointSecrets :exec
-- A second rotation inside the grace window drops the older retired secret:
-- only the immediately previous secret may co-sign deliveries.
UPDATE webhook_endpoint_secrets
SET grace_expires_at = NOW()
WHERE tenant_id = @tenant_id
  AND endpoint_id = @endpoint_id
  AND NOT active
  AND grace_expires_at > NOW();

-- name: DeactivateWebhookEndpointSecrets :exec
-- Retires the active secret but keeps it usable for dual-signing until its
-- explicit grace expiry.
UPDATE webhook_endpoint_secrets
SET active = FALSE,
    grace_expires_at = NOW() + make_interval(secs => @grace_seconds::double precision)
WHERE tenant_id = @tenant_id AND endpoint_id = @endpoint_id AND active;

-- name: CreateWebhookEndpointSecret :one
INSERT INTO webhook_endpoint_secrets (
    tenant_id, endpoint_id, version, secret_ciphertext, active
)
VALUES (
    @tenant_id, @endpoint_id, @version, @secret_ciphertext, TRUE
)
RETURNING *;

-- name: GetActiveWebhookEndpointSecret :one
SELECT * FROM webhook_endpoint_secrets
WHERE tenant_id = @tenant_id AND endpoint_id = @endpoint_id AND active
LIMIT 1;

-- name: GetWebhookEndpointSecretByID :one
-- A queued delivery references the exact secret version it snapshotted; the
-- worker reads that row, never a ciphertext copied onto the delivery.
SELECT * FROM webhook_endpoint_secrets
WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetWebhookEndpointSigningSecrets :one
-- The delivery worker signs with the current secret first and, while its
-- explicit grace expiry is still open, the immediately previous one too.
SELECT
    COALESCE((SELECT s.secret_ciphertext FROM webhook_endpoint_secrets s
      WHERE s.tenant_id = @tenant_id AND s.endpoint_id = @endpoint_id AND s.active
      ORDER BY s.version DESC LIMIT 1), '')::text AS current_ciphertext,
    COALESCE((SELECT s.secret_ciphertext FROM webhook_endpoint_secrets s
      WHERE s.tenant_id = @tenant_id AND s.endpoint_id = @endpoint_id
        AND NOT s.active AND s.grace_expires_at > NOW()
      ORDER BY s.version DESC LIMIT 1), '')::text AS previous_ciphertext;

-- name: LockWebhookEndpoint :one
-- Serializes concurrent secret rotations: the row lock is held until the
-- rotation transaction commits, so NextWebhookEndpointSecretVersion cannot
-- hand two rotations the same version.
SELECT sqlc.embed(e),
       ARRAY(SELECT s.pattern FROM webhook_endpoint_subscriptions s
             WHERE s.tenant_id = e.tenant_id AND s.endpoint_id = e.id
             ORDER BY s.pattern COLLATE "C")::text[] AS event_types
FROM webhook_endpoints e
WHERE e.id = @id AND e.tenant_id = @tenant_id AND e.deleted_at IS NULL
FOR UPDATE;

-- name: NextWebhookEndpointSecretVersion :one
SELECT (COALESCE(MAX(version), 0) + 1)::integer AS version
FROM webhook_endpoint_secrets
WHERE endpoint_id = @endpoint_id;

-- name: ListActiveWebhookEndpointsWithSecret :many
SELECT
    e.id,
    e.tenant_id,
    e.url,
    e.max_attempts,
    e.timeout_ms,
    ARRAY(SELECT sub.pattern FROM webhook_endpoint_subscriptions sub
          WHERE sub.tenant_id = e.tenant_id AND sub.endpoint_id = e.id
          ORDER BY sub.pattern COLLATE "C")::text[] AS event_types,
    s.id AS secret_id,
    s.secret_ciphertext AS secret_ciphertext
FROM webhook_endpoints e
JOIN webhook_endpoint_secrets s
  ON s.endpoint_id = e.id AND s.tenant_id = e.tenant_id AND s.active
WHERE e.tenant_id = @tenant_id
  AND e.deleted_at IS NULL
  AND e.status = 'active'
ORDER BY e.id;

-- Events and deliveries -----------------------------------------------------

-- name: CreateWebhookEvent :exec
-- The first delivery of an event writes it; every later endpoint (and every
-- retry of the subscriber job) finds it already present. An event id is
-- immutable once written, so the existing row is kept as is.
INSERT INTO webhook_events (
    tenant_id, event_id, event_type, data_version, actor_type, actor_id,
    subject_user_ids, occurred_at, payload
)
VALUES (
    @tenant_id, @event_id, @event_type, @data_version, @actor_type,
    sqlc.narg(actor_id), @subject_user_ids::uuid[], @occurred_at, @payload::jsonb
)
ON CONFLICT (tenant_id, event_id) DO NOTHING;

-- name: CreateWebhookDelivery :one
INSERT INTO webhook_deliveries (
    tenant_id, endpoint_id, event_id, secret_id,
    url, status, max_attempts, timeout_ms,
    next_attempt_at, replayed_from_id, is_replay
)
VALUES (
    @tenant_id, @endpoint_id, @event_id, @secret_id,
    @url, 'pending', @max_attempts, @timeout_ms,
    NOW(), @replayed_from_id, @is_replay
)
ON CONFLICT (tenant_id, endpoint_id, event_id) WHERE NOT is_replay DO NOTHING
RETURNING id;

-- name: GetWebhookDelivery :one
SELECT sqlc.embed(d), ev.event_type, ev.data_version, ev.actor_type, ev.actor_id,
       ev.occurred_at, ev.payload
FROM webhook_deliveries d
JOIN webhook_events ev ON ev.tenant_id = d.tenant_id AND ev.event_id = d.event_id
WHERE d.id = @id AND d.tenant_id = @tenant_id;

-- name: ListWebhookDeliveries :many
-- The list response carries no payload, so it is not read.
SELECT sqlc.embed(d), ev.event_type, ev.occurred_at
FROM webhook_deliveries d
JOIN webhook_events ev ON ev.tenant_id = d.tenant_id AND ev.event_id = d.event_id
WHERE d.tenant_id = @tenant_id
  AND (sqlc.narg(endpoint_id)::uuid IS NULL OR d.endpoint_id = sqlc.narg(endpoint_id)::uuid)
  AND (@status::text = '' OR d.status = @status)
  AND (d.created_at, d.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY d.created_at DESC, d.id DESC
LIMIT @row_limit::int;

-- name: ClaimWebhookDelivery :one
-- A disabled or deleted endpoint is never claimed, so it stops receiving
-- retries; ExpireUnclaimableWebhookDelivery then dead-letters the delivery.
-- The event is joined in so the worker builds the envelope from one read.
UPDATE webhook_deliveries
SET attempts = attempts + 1,
    next_attempt_at = NOW() + make_interval(secs => @lease_seconds::double precision)
FROM webhook_events ev
WHERE webhook_deliveries.id = @id
  AND webhook_deliveries.tenant_id = @tenant_id
  AND ev.tenant_id = webhook_deliveries.tenant_id
  AND ev.event_id = webhook_deliveries.event_id
  AND webhook_deliveries.status = 'pending'
  AND webhook_deliveries.attempts < webhook_deliveries.max_attempts
  AND webhook_deliveries.next_attempt_at <= NOW()
  AND EXISTS (
      SELECT 1 FROM webhook_endpoints e
      WHERE e.id = webhook_deliveries.endpoint_id
        AND e.tenant_id = webhook_deliveries.tenant_id
        AND e.status = 'active'
        AND e.deleted_at IS NULL
  )
RETURNING webhook_deliveries.*, ev.event_type, ev.data_version, ev.actor_type,
          ev.actor_id, ev.occurred_at, ev.payload;

-- name: ExpireUnclaimableWebhookDelivery :execrows
-- Dead-letters a pending delivery that can never be claimed again: its
-- endpoint is disabled/deleted, or every attempt was consumed but the final
-- bookkeeping write was lost. A live claim lease (next_attempt_at in the
-- future) is left alone so an in-flight attempt can finish.
UPDATE webhook_deliveries
SET status = 'dead',
    next_attempt_at = NOW(),
    last_error = CASE
        WHEN attempts >= max_attempts THEN 'delivery attempts exhausted'
        ELSE 'endpoint is disabled or deleted'
    END
WHERE webhook_deliveries.id = @id
  AND webhook_deliveries.tenant_id = @tenant_id
  AND status = 'pending'
  AND next_attempt_at <= NOW()
  AND (
      attempts >= max_attempts
      OR NOT EXISTS (
          SELECT 1 FROM webhook_endpoints e
          WHERE e.id = webhook_deliveries.endpoint_id
            AND e.tenant_id = webhook_deliveries.tenant_id
            AND e.status = 'active'
            AND e.deleted_at IS NULL
      )
  );

-- The outcome writes below are fenced by the attempt the worker claimed
-- (attempts = @attempts): when a claim lease expired and another worker
-- claimed the next attempt, the stale worker's write affects no row instead of
-- overwriting the newer attempt's result.

-- name: MarkWebhookDeliverySucceeded :execrows
UPDATE webhook_deliveries
SET status = 'succeeded',
    delivered_at = NOW(),
    next_attempt_at = NOW(),
    last_status_code = NULLIF(@status_code::integer, 0),
    last_error = NULL
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending'
  AND attempts = @attempts;

-- name: MarkWebhookDeliveryDead :execrows
UPDATE webhook_deliveries
SET status = 'dead',
    next_attempt_at = NOW(),
    last_status_code = NULLIF(@status_code::integer, 0),
    last_error = @last_error
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending'
  AND attempts = @attempts;

-- name: RescheduleWebhookDelivery :execrows
UPDATE webhook_deliveries
SET status = 'pending',
    next_attempt_at = @next_attempt_at,
    last_status_code = NULLIF(@status_code::integer, 0),
    last_error = @last_error
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending'
  AND attempts = @attempts;

-- name: ReleaseWebhookDeliveryClaim :execrows
-- Gives back a claimed attempt that was interrupted by worker shutdown before
-- a response arrived, so a drain on the final attempt cannot dead-letter a
-- delivery that may never have been sent. The claim lease is cleared so the
-- requeued job can claim it again immediately.
UPDATE webhook_deliveries
SET attempts = attempts - 1,
    next_attempt_at = NOW()
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending'
  AND attempts = @attempts AND attempts > 0;

-- name: CreateWebhookDeliveryAttempt :one
INSERT INTO webhook_delivery_attempts (
    tenant_id, delivery_id, attempt, status_code, duration_ms, error
)
VALUES (
    @tenant_id, @delivery_id, @attempt, NULLIF(@status_code::integer, 0),
    @duration_ms, @error
)
ON CONFLICT (delivery_id, attempt) DO NOTHING
RETURNING id;

-- name: ListWebhookDeliveryAttempts :many
SELECT * FROM webhook_delivery_attempts
WHERE tenant_id = @tenant_id AND delivery_id = @delivery_id
ORDER BY attempt ASC;

-- name: PurgeWebhookDeliveries :execrows
-- One bounded retention batch, served by idx_webhook_deliveries_terminal_updated_at.
-- The caller loops until a batch is short.
WITH batch AS (
    SELECT id
    FROM webhook_deliveries
    WHERE status IN ('succeeded', 'dead')
      AND updated_at < NOW() - make_interval(days => @retention_days::integer)
    ORDER BY updated_at
    LIMIT @batch_size::integer
    FOR UPDATE SKIP LOCKED
)
DELETE FROM webhook_deliveries AS d
USING batch
WHERE d.id = batch.id;

-- name: PurgeOrphanWebhookEvents :execrows
-- One bounded retention batch of events no delivery references any more (run
-- after PurgeWebhookDeliveries). The age bound keeps the pass away from
-- events still being fanned out, and SKIP LOCKED skips an event whose new
-- delivery's FK check holds a key-share lock on it; the RESTRICT FK is the
-- final guard. The caller loops until a batch is short.
WITH batch AS (
    SELECT ev.tenant_id, ev.event_id
    FROM webhook_events AS ev
    WHERE ev.created_at < NOW() - make_interval(days => @retention_days::integer)
      AND NOT EXISTS (
          SELECT 1 FROM webhook_deliveries d
          WHERE d.tenant_id = ev.tenant_id AND d.event_id = ev.event_id
      )
    ORDER BY ev.created_at
    LIMIT @batch_size::integer
    FOR UPDATE SKIP LOCKED
)
DELETE FROM webhook_events AS ev
USING batch
WHERE ev.tenant_id = batch.tenant_id AND ev.event_id = batch.event_id;

-- Inbound sources and events ------------------------------------------------

-- name: CreateWebhookInboundSource :one
-- The source and its first secret are separate rows: the caller creates the
-- source, seals the secret (its AAD binds the source id), then inserts the
-- version-1 secret in the same transaction.
INSERT INTO webhook_inbound_sources (
    tenant_id, name, slug, status
)
VALUES (
    @tenant_id, @name, @slug, @status
)
RETURNING *;

-- name: CreateWebhookInboundSourceSecret :one
INSERT INTO webhook_inbound_source_secrets (
    tenant_id, source_id, version, secret_ciphertext, active
)
VALUES (
    @tenant_id, @source_id, @version, @secret_ciphertext, TRUE
)
RETURNING *;

-- name: GetWebhookInboundSource :one
SELECT * FROM webhook_inbound_sources
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: GetWebhookInboundSourceByID :one
SELECT * FROM webhook_inbound_sources
WHERE id = @id AND deleted_at IS NULL;

-- name: LockWebhookInboundSource :one
-- Serializes concurrent secret rotations: the row lock is held until the
-- rotation transaction commits, so NextWebhookInboundSourceSecretVersion
-- cannot hand two rotations the same version.
SELECT * FROM webhook_inbound_sources
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
FOR UPDATE;

-- name: ListWebhookInboundSources :many
SELECT * FROM webhook_inbound_sources
WHERE tenant_id = @tenant_id
  AND deleted_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: UpdateWebhookInboundSource :one
UPDATE webhook_inbound_sources
SET name = @name, slug = @slug, status = @status
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
RETURNING *;

-- name: DeleteWebhookInboundSource :one
UPDATE webhook_inbound_sources
SET status = 'disabled', deleted_at = NOW()
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
RETURNING *;

-- name: ExpireOlderWebhookInboundSourceSecrets :exec
-- A second rotation inside the grace window drops the older retired secret:
-- only the immediately previous secret may co-sign inbound verification.
UPDATE webhook_inbound_source_secrets
SET grace_expires_at = NOW()
WHERE tenant_id = @tenant_id
  AND source_id = @source_id
  AND NOT active
  AND grace_expires_at > NOW();

-- name: DeactivateWebhookInboundSourceSecrets :exec
-- Retires the active secret but keeps it verifiable until its explicit grace
-- expiry.
UPDATE webhook_inbound_source_secrets
SET active = FALSE,
    grace_expires_at = NOW() + make_interval(secs => @grace_seconds::double precision)
WHERE tenant_id = @tenant_id AND source_id = @source_id AND active;

-- name: NextWebhookInboundSourceSecretVersion :one
SELECT (COALESCE(MAX(version), 0) + 1)::integer AS version
FROM webhook_inbound_source_secrets
WHERE source_id = @source_id;

-- name: GetWebhookInboundSourceSigningSecrets :one
-- The receiver verifies with the current secret first and, while its explicit
-- grace expiry is still open, the immediately previous one too.
SELECT
    COALESCE((SELECT s.secret_ciphertext FROM webhook_inbound_source_secrets s
      WHERE s.tenant_id = @tenant_id AND s.source_id = @source_id AND s.active
      ORDER BY s.version DESC LIMIT 1), '')::text AS current_ciphertext,
    COALESCE((SELECT s.secret_ciphertext FROM webhook_inbound_source_secrets s
      WHERE s.tenant_id = @tenant_id AND s.source_id = @source_id
        AND NOT s.active AND s.grace_expires_at > NOW()
      ORDER BY s.version DESC LIMIT 1), '')::text AS previous_ciphertext;

-- name: CreateWebhookInboundEvent :one
INSERT INTO webhook_inbound_events (
    tenant_id, source_id, external_event_id, event_type, signature, payload
)
VALUES (
    @tenant_id, @source_id, @external_event_id, @event_type, @signature, @payload::jsonb
)
ON CONFLICT (tenant_id, source_id, external_event_id) DO NOTHING
RETURNING id;

-- name: GetWebhookInboundEventByExternalID :one
SELECT * FROM webhook_inbound_events
WHERE tenant_id = @tenant_id
  AND source_id = @source_id
  AND external_event_id = @external_event_id;

-- name: ListWebhookInboundEvents :many
SELECT * FROM webhook_inbound_events
WHERE tenant_id = @tenant_id AND source_id = @source_id
  AND (received_at, id) < (@cursor_received_at::timestamptz, @cursor_id::uuid)
ORDER BY received_at DESC, id DESC
LIMIT @row_limit::int;

-- name: PurgeWebhookInboundEvents :execrows
-- One bounded retention batch, served by idx_webhook_inbound_events_received_at.
-- The caller loops until a batch is short.
WITH batch AS (
    SELECT id
    FROM webhook_inbound_events
    WHERE received_at < NOW() - make_interval(days => @retention_days::integer)
    ORDER BY received_at
    LIMIT @batch_size::integer
    FOR UPDATE SKIP LOCKED
)
DELETE FROM webhook_inbound_events AS e
USING batch
WHERE e.id = batch.id;

-- name: UpdateWebhookEndpointIfUnchanged :one
-- The ETag compare-and-swap for an endpoint update. A concurrent writer makes
-- this return no rows (412) instead of overwriting the other change.
UPDATE webhook_endpoints
SET url = @url,
    description = @description,
    status = @status,
    max_attempts = @max_attempts,
    timeout_ms = @timeout_ms
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
  AND updated_at = @expected_updated_at
RETURNING *;

-- name: UpdateWebhookInboundSourceIfUnchanged :one
-- The ETag compare-and-swap for an inbound source update.
UPDATE webhook_inbound_sources
SET name = @name,
    slug = @slug,
    status = @status
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
  AND updated_at = @expected_updated_at
RETURNING *;
