-- name: CreateNotification :one
INSERT INTO notifications (tenant_id, user_id, type, title, body, data)
VALUES (@tenant_id::bigint, @user_id::bigint, @type::varchar, @title::varchar, @body::text, @data::jsonb)
RETURNING *;

-- name: GetNotificationByID :one
SELECT * FROM notifications
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint;

-- name: ListNotificationsCursor :many
SELECT * FROM notifications
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND (NOT @unread_only::boolean OR read_at IS NULL)
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: CountUnreadNotifications :one
SELECT count(*) FROM notifications
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND read_at IS NULL;

-- name: MarkNotificationRead :one
UPDATE notifications
SET read_at = COALESCE(read_at, NOW())
WHERE id = @id::bigint
  AND tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
RETURNING *;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications
SET read_at = NOW()
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND read_at IS NULL;

-- name: GetNotificationPreference :one
SELECT * FROM notification_preferences
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
  AND type = @type::varchar
  AND channel = @channel::varchar;

-- name: ListNotificationPreferences :many
SELECT * FROM notification_preferences
WHERE tenant_id = @tenant_id::bigint
  AND user_id = @user_id::bigint
ORDER BY type ASC, channel ASC;

-- name: UpsertNotificationPreference :one
INSERT INTO notification_preferences (tenant_id, user_id, type, channel, enabled)
VALUES (@tenant_id::bigint, @user_id::bigint, @type::varchar, @channel::varchar, @enabled::boolean)
ON CONFLICT (tenant_id, user_id, type, channel)
DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()
RETURNING *;
