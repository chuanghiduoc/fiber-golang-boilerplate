-- Tenant purge. Deliveries go before events and events before endpoints
-- because both snapshot foreign keys are RESTRICT; attempts, subscriptions,
-- and secrets cascade from their parent. Every table is drained in bounded
-- id (or tenant/event key) batches.

-- name: DeleteTenantWebhookDeliveriesBatch :execrows
DELETE FROM webhook_deliveries
WHERE id IN (
    SELECT id FROM webhook_deliveries
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);

-- name: DeleteTenantWebhookEventsBatch :execrows
DELETE FROM webhook_events
WHERE tenant_id = @tenant_id::uuid
  AND event_id IN (
      SELECT event_id FROM webhook_events
      WHERE tenant_id = @tenant_id::uuid
      LIMIT @batch_size::int
  );

-- name: DeleteTenantWebhookEndpointsBatch :execrows
DELETE FROM webhook_endpoints
WHERE id IN (
    SELECT id FROM webhook_endpoints
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);

-- name: DeleteTenantWebhookInboundEventsBatch :execrows
DELETE FROM webhook_inbound_events
WHERE id IN (
    SELECT id FROM webhook_inbound_events
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);

-- name: DeleteTenantWebhookInboundSourcesBatch :execrows
DELETE FROM webhook_inbound_sources
WHERE id IN (
    SELECT id FROM webhook_inbound_sources
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);
