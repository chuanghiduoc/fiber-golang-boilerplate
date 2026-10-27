-- Tenant purge. Preferences and feed rows are erased in bounded id batches,
-- preferences first so a failure leaves a resumable state.

-- name: DeleteTenantNotificationPreferencesBatch :execrows
-- Preferences have a natural primary key, not a surrogate id: delete one
-- bounded page selected by its natural key.
DELETE FROM notification_preferences
WHERE (tenant_id, user_id, type, channel) IN (
    SELECT tenant_id, user_id, type, channel FROM notification_preferences
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);

-- name: DeleteTenantNotificationsBatch :execrows
DELETE FROM notifications
WHERE id IN (
    SELECT id FROM notifications
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);
