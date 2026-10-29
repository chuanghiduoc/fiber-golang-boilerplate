-- owner: webhooks

-- Privacy export/erasure used one OR predicate over subject, actor, and two
-- payload keys. Each UNION branch now has a supporting index, and every
-- branch carries the (occurred_at, event_id) keyset so a page can stop without
-- scanning the tenant's whole event history.
CREATE INDEX idx_webhook_events_tenant_occurred_at_event
    ON webhook_events (tenant_id, occurred_at DESC, event_id DESC);

CREATE INDEX idx_webhook_events_tenant_actor_occurred_at_event
    ON webhook_events (tenant_id, actor_type, actor_id, occurred_at DESC, event_id DESC)
    WHERE actor_type = 'user' AND actor_id IS NOT NULL;

CREATE INDEX idx_webhook_events_tenant_payload_user_occurred_at_event
    ON webhook_events (tenant_id, (payload->>'userId'), occurred_at DESC, event_id DESC);

CREATE INDEX idx_webhook_events_tenant_payload_owner_occurred_at_event
    ON webhook_events (tenant_id, (payload->>'ownerId'), occurred_at DESC, event_id DESC);
