-- Privacy export/erasure for stored webhook event history attributed to one
-- user. Endpoint configuration is tenant-wide and is not touched.

-- name: ListUserWebhookEvents :many
WITH candidate_events AS (
    (SELECT tenant_id, event_id, occurred_at
     FROM webhook_events
     WHERE tenant_id = @tenant_id::uuid
       AND subject_user_ids @> ARRAY[@user_id::uuid]
       AND (occurred_at, event_id) < (@cursor_occurred_at::timestamptz, @cursor_id::uuid)
     ORDER BY occurred_at DESC, event_id DESC
     LIMIT @row_limit::int)
    UNION ALL
    (SELECT tenant_id, event_id, occurred_at
     FROM webhook_events
     WHERE tenant_id = @tenant_id::uuid
       AND actor_type = 'user'
       AND actor_id = @user_id::uuid
       AND (occurred_at, event_id) < (@cursor_occurred_at::timestamptz, @cursor_id::uuid)
     ORDER BY occurred_at DESC, event_id DESC
     LIMIT @row_limit::int)
    UNION ALL
    (SELECT tenant_id, event_id, occurred_at
     FROM webhook_events
     WHERE tenant_id = @tenant_id::uuid
       AND payload->>'userId' = @user_id::text
       AND (occurred_at, event_id) < (@cursor_occurred_at::timestamptz, @cursor_id::uuid)
     ORDER BY occurred_at DESC, event_id DESC
     LIMIT @row_limit::int)
    UNION ALL
    (SELECT tenant_id, event_id, occurred_at
     FROM webhook_events
     WHERE tenant_id = @tenant_id::uuid
       AND payload->>'ownerId' = @user_id::text
       AND (occurred_at, event_id) < (@cursor_occurred_at::timestamptz, @cursor_id::uuid)
     ORDER BY occurred_at DESC, event_id DESC
     LIMIT @row_limit::int)
),
matched_events AS (
    SELECT DISTINCT tenant_id, event_id, occurred_at
    FROM candidate_events
)
SELECT e.*
FROM webhook_events e
JOIN matched_events m ON m.tenant_id = e.tenant_id AND m.event_id = e.event_id
WHERE e.tenant_id = @tenant_id::uuid
ORDER BY m.occurred_at DESC, m.event_id DESC
LIMIT @row_limit::int;

-- name: ListUserWebhookDeliveries :many
WITH candidate_deliveries AS (
    (SELECT d.*
     FROM webhook_deliveries d
     JOIN webhook_events e
       ON e.tenant_id = d.tenant_id
      AND e.event_id = d.event_id
     WHERE d.tenant_id = @tenant_id::uuid
       AND e.subject_user_ids @> ARRAY[@user_id::uuid]
       AND (d.created_at, d.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
     ORDER BY d.created_at DESC, d.id DESC
     LIMIT @row_limit::int)
    UNION
    (SELECT d.*
     FROM webhook_deliveries d
     JOIN webhook_events e
       ON e.tenant_id = d.tenant_id
      AND e.event_id = d.event_id
     WHERE d.tenant_id = @tenant_id::uuid
       AND e.actor_type = 'user'
       AND e.actor_id = @user_id::uuid
       AND (d.created_at, d.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
     ORDER BY d.created_at DESC, d.id DESC
     LIMIT @row_limit::int)
    UNION
    (SELECT d.*
     FROM webhook_deliveries d
     JOIN webhook_events e
       ON e.tenant_id = d.tenant_id
      AND e.event_id = d.event_id
     WHERE d.tenant_id = @tenant_id::uuid
       AND e.payload->>'userId' = @user_id::text
       AND (d.created_at, d.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
     ORDER BY d.created_at DESC, d.id DESC
     LIMIT @row_limit::int)
    UNION
    (SELECT d.*
     FROM webhook_deliveries d
     JOIN webhook_events e
       ON e.tenant_id = d.tenant_id
      AND e.event_id = d.event_id
     WHERE d.tenant_id = @tenant_id::uuid
       AND e.payload->>'ownerId' = @user_id::text
       AND (d.created_at, d.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
     ORDER BY d.created_at DESC, d.id DESC
     LIMIT @row_limit::int)
)
SELECT *
FROM candidate_deliveries
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: DeleteUserWebhookDeliveries :exec
-- Attempts cascade; deleting deliveries first satisfies the event RESTRICT FK.
DELETE FROM webhook_deliveries d
USING (
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND subject_user_ids @> ARRAY[@user_id::uuid]
    UNION
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND actor_type = 'user'
      AND actor_id = @user_id::uuid
    UNION
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND payload->>'userId' = @user_id::text
    UNION
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND payload->>'ownerId' = @user_id::text
) matched
WHERE d.tenant_id = @tenant_id::uuid
  AND matched.tenant_id = d.tenant_id
  AND matched.event_id = d.event_id;

-- name: DeleteUserWebhookEvents :exec
DELETE FROM webhook_events e
USING (
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND subject_user_ids @> ARRAY[@user_id::uuid]
    UNION
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND actor_type = 'user'
      AND actor_id = @user_id::uuid
    UNION
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND payload->>'userId' = @user_id::text
    UNION
    SELECT tenant_id, event_id
    FROM webhook_events
    WHERE tenant_id = @tenant_id::uuid
      AND payload->>'ownerId' = @user_id::text
) matched
WHERE e.tenant_id = @tenant_id::uuid
  AND matched.tenant_id = e.tenant_id
  AND matched.event_id = e.event_id;
