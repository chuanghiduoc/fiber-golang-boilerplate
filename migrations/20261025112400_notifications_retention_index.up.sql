-- notifications: read-notification retention (notifications.retention.purge).
--
-- The purge is a deliberate cross-tenant maintenance pass that selects read
-- rows by read_at, so this index leads with read_at instead of tenant_id. It
-- is partial: unread rows are never purged and are not indexed. CONCURRENTLY
-- avoids blocking writes on a live notifications table; golang-migrate runs
-- this single statement outside a transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_notifications_read_at
    ON notifications (read_at)
    WHERE read_at IS NOT NULL;
