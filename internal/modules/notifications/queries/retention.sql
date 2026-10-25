-- name: PurgeReadNotifications :execrows
-- Retention (notifications.retention.purge): a deliberate cross-tenant
-- maintenance pass, the only notifications query without a tenant_id
-- predicate. Deletes one bounded batch of notifications read more than
-- @retention_days ago; unread rows are never touched. SKIP LOCKED avoids
-- waiting on rows a concurrent purge or mark-read holds. Served by
-- idx_notifications_read_at.
WITH batch AS (
    SELECT id FROM notifications
    WHERE read_at IS NOT NULL
      AND read_at < now() - make_interval(days => @retention_days::int)
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM notifications AS n
USING batch
WHERE n.id = batch.id;
