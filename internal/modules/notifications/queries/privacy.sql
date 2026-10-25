-- Privacy export/erasure for the notification feed and preferences. The
-- tenant and user are explicit because the privacy contract passes them.

-- name: ListNotificationsForPrivacy :many
SELECT * FROM notifications
WHERE tenant_id = $1 AND user_id = $2
ORDER BY id;

-- name: ListPreferencesForPrivacy :many
SELECT * FROM notification_preferences
WHERE tenant_id = $1 AND user_id = $2
ORDER BY id;

-- name: DeleteNotificationPreferencesForPrivacy :exec
DELETE FROM notification_preferences
WHERE tenant_id = $1 AND user_id = $2;

-- name: DeleteNotificationsForPrivacy :exec
DELETE FROM notifications
WHERE tenant_id = $1 AND user_id = $2;
