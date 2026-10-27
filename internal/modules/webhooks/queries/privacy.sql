-- Privacy export/erasure for stored webhook event history attributed to one
-- user. Endpoint configuration is tenant-wide and is not touched.

-- name: ListUserWebhookEvents :many
SELECT * FROM webhook_events
WHERE tenant_id = @tenant_id::uuid
  AND ((actor_type = 'user' AND actor_id = @user_id::uuid)
       OR payload->>'userId' = @user_id::text
       OR payload->>'ownerId' = @user_id::text)
ORDER BY occurred_at DESC, event_id;

-- name: ListUserWebhookDeliveries :many
SELECT d.* FROM webhook_deliveries d
JOIN webhook_events e ON e.tenant_id = d.tenant_id AND e.event_id = d.event_id
WHERE d.tenant_id = @tenant_id::uuid
  AND ((e.actor_type = 'user' AND e.actor_id = @user_id::uuid)
       OR e.payload->>'userId' = @user_id::text
       OR e.payload->>'ownerId' = @user_id::text)
ORDER BY d.created_at DESC, d.id DESC;

-- name: DeleteUserWebhookDeliveries :exec
-- Attempts cascade; deleting deliveries first satisfies the event RESTRICT FK.
DELETE FROM webhook_deliveries d
USING webhook_events e
WHERE d.tenant_id = @tenant_id::uuid
  AND e.tenant_id = d.tenant_id AND e.event_id = d.event_id
  AND ((e.actor_type = 'user' AND e.actor_id = @user_id::uuid)
       OR e.payload->>'userId' = @user_id::text
       OR e.payload->>'ownerId' = @user_id::text);

-- name: DeleteUserWebhookEvents :exec
DELETE FROM webhook_events
WHERE tenant_id = @tenant_id::uuid
  AND ((actor_type = 'user' AND actor_id = @user_id::uuid)
       OR payload->>'userId' = @user_id::text
       OR payload->>'ownerId' = @user_id::text);
