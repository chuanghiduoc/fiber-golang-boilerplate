-- Privacy export/erasure for the notification feed and preferences. The
-- tenant and user are explicit because the privacy contract passes them.

-- name: ListNotificationsForPrivacy :many
SELECT * FROM notifications
WHERE tenant_id = $1 AND user_id = $2
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: ListPreferencesForPrivacy :many
SELECT * FROM notification_preferences
WHERE tenant_id = $1 AND user_id = $2
ORDER BY type, channel
LIMIT @row_limit::int;

-- name: DeleteAllNotificationPrivacy :exec
-- One statement so a failed erasure cannot leave notifications without their
-- preferences (or the reverse): a data-modifying CTE runs both deletes in the
-- same transaction, even though it does not reference the CTE result.
WITH removed_preferences AS (
    DELETE FROM notification_preferences AS np
    WHERE np.tenant_id = $1 AND np.user_id = $2
)
DELETE FROM notifications AS n
WHERE n.tenant_id = $1 AND n.user_id = $2;
