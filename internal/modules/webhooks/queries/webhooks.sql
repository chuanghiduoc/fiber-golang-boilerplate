-- Outbound endpoints -------------------------------------------------------

-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints (
    tenant_id, url, description, event_types, status, max_attempts, timeout_ms
)
VALUES (
    @tenant_id, @url, @description, @event_types::text[], @status, @max_attempts, @timeout_ms
)
RETURNING *;

-- name: GetWebhookEndpoint :one
SELECT * FROM webhook_endpoints
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: ListWebhookEndpoints :many
SELECT * FROM webhook_endpoints
WHERE tenant_id = @tenant_id
  AND deleted_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: UpdateWebhookEndpoint :one
UPDATE webhook_endpoints
SET url = @url,
    description = @description,
    event_types = @event_types::text[],
    status = @status,
    max_attempts = @max_attempts,
    timeout_ms = @timeout_ms
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
RETURNING *;

-- name: DeleteWebhookEndpoint :one
UPDATE webhook_endpoints
SET status = 'disabled', deleted_at = NOW()
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
RETURNING *;

-- name: DeactivateWebhookEndpointSecrets :exec
UPDATE webhook_endpoint_secrets
SET active = FALSE
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

-- name: LockWebhookEndpoint :one
-- Serializes concurrent secret rotations: the row lock is held until the
-- rotation transaction commits, so NextWebhookEndpointSecretVersion cannot
-- hand two rotations the same version.
SELECT * FROM webhook_endpoints
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
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
    e.description,
    e.event_types,
    e.status,
    e.max_attempts,
    e.timeout_ms,
    e.created_at,
    e.updated_at,
    e.deleted_at,
    s.id AS secret_id,
    s.version AS secret_version,
    s.secret_ciphertext AS secret_ciphertext
FROM webhook_endpoints e
JOIN webhook_endpoint_secrets s
  ON s.endpoint_id = e.id AND s.tenant_id = e.tenant_id AND s.active
WHERE e.tenant_id = @tenant_id
  AND e.deleted_at IS NULL
  AND e.status = 'active'
ORDER BY e.id;

-- Deliveries ----------------------------------------------------------------

-- name: CreateWebhookDelivery :one
INSERT INTO webhook_deliveries (
    tenant_id, endpoint_id, event_id, event_type, event_created_at,
    url, payload, secret_ciphertext, status, max_attempts, timeout_ms,
    next_attempt_at, replayed_from_id
)
VALUES (
    @tenant_id, @endpoint_id, @event_id, @event_type, @event_created_at,
    @url, @payload::jsonb, @secret_ciphertext, 'pending', @max_attempts, @timeout_ms,
    NOW(), @replayed_from_id
)
ON CONFLICT (tenant_id, endpoint_id, event_id) DO NOTHING
RETURNING id;

-- name: GetWebhookDelivery :one
SELECT * FROM webhook_deliveries
WHERE id = @id AND tenant_id = @tenant_id;

-- name: ListWebhookDeliveries :many
SELECT * FROM webhook_deliveries
WHERE tenant_id = @tenant_id
  AND (@endpoint_id::bigint = 0 OR endpoint_id = @endpoint_id)
  AND (@status::text = '' OR status = @status)
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: ClaimWebhookDelivery :one
-- A disabled or deleted endpoint is never claimed, so it stops receiving
-- retries; ExpireUnclaimableWebhookDelivery then dead-letters the delivery.
UPDATE webhook_deliveries
SET attempts = attempts + 1,
    next_attempt_at = NOW() + make_interval(secs => @lease_seconds::double precision)
WHERE webhook_deliveries.id = @id
  AND webhook_deliveries.tenant_id = @tenant_id
  AND status = 'pending'
  AND attempts < max_attempts
  AND next_attempt_at <= NOW()
  AND EXISTS (
      SELECT 1 FROM webhook_endpoints e
      WHERE e.id = webhook_deliveries.endpoint_id
        AND e.tenant_id = webhook_deliveries.tenant_id
        AND e.status = 'active'
        AND e.deleted_at IS NULL
  )
RETURNING *;

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

-- name: MarkWebhookDeliverySucceeded :exec
UPDATE webhook_deliveries
SET status = 'succeeded',
    delivered_at = NOW(),
    next_attempt_at = NOW(),
    last_status_code = NULLIF(@status_code::integer, 0),
    last_error = NULL
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending';

-- name: MarkWebhookDeliveryDead :exec
UPDATE webhook_deliveries
SET status = 'dead',
    next_attempt_at = NOW(),
    last_status_code = NULLIF(@status_code::integer, 0),
    last_error = @last_error
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending';

-- name: RescheduleWebhookDelivery :execrows
UPDATE webhook_deliveries
SET status = 'pending',
    next_attempt_at = @next_attempt_at,
    last_status_code = NULLIF(@status_code::integer, 0),
    last_error = @last_error
WHERE id = @id AND tenant_id = @tenant_id AND status = 'pending';

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

-- name: CountWebhookDeliveriesByStatus :one
SELECT
    count(*) FILTER (WHERE status = 'pending')::bigint AS pending,
    count(*) FILTER (WHERE status = 'succeeded')::bigint AS succeeded,
    count(*) FILTER (WHERE status = 'dead')::bigint AS dead
FROM webhook_deliveries
WHERE tenant_id = @tenant_id;

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

-- Inbound sources and events ------------------------------------------------

-- name: CreateWebhookInboundSource :one
INSERT INTO webhook_inbound_sources (
    tenant_id, name, slug, secret_ciphertext, status
)
VALUES (
    @tenant_id, @name, @slug, @secret_ciphertext, 'active'
)
RETURNING *;

-- name: GetWebhookInboundSource :one
SELECT * FROM webhook_inbound_sources
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL;

-- name: GetWebhookInboundSourceByID :one
SELECT * FROM webhook_inbound_sources
WHERE id = @id AND deleted_at IS NULL;

-- name: ListWebhookInboundSources :many
SELECT * FROM webhook_inbound_sources
WHERE tenant_id = @tenant_id
  AND deleted_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
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

-- name: RotateWebhookInboundSourceSecret :one
UPDATE webhook_inbound_sources
SET secret_ciphertext = @secret_ciphertext
WHERE id = @id AND tenant_id = @tenant_id AND deleted_at IS NULL
RETURNING *;

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
  AND (received_at, id) < (@cursor_received_at::timestamptz, @cursor_id::bigint)
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
