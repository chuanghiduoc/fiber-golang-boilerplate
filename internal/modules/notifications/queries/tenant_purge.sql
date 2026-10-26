-- Tenant purge. Preferences and feed rows are erased in bounded id batches,
-- preferences first so a failure leaves a resumable state.

-- name: DeleteTenantNotificationPreferencesBatch :execrows
DELETE FROM notification_preferences
WHERE id IN (
    SELECT id FROM notification_preferences
    WHERE tenant_id = @tenant_id::bigint
    LIMIT @batch_size::int
);

-- name: DeleteTenantNotificationsBatch :execrows
DELETE FROM notifications
WHERE id IN (
    SELECT id FROM notifications
    WHERE tenant_id = @tenant_id::bigint
    LIMIT @batch_size::int
);
