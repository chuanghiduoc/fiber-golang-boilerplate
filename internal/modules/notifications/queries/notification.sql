-- name: CreateNotification :one
INSERT INTO notifications (tenant_id, user_id, type, title, body, data)
VALUES (@tenant_id::uuid, @user_id::uuid, @type::text, @title::text, @body::text, @data::jsonb)
RETURNING *;

-- name: GetNotificationByID :one
SELECT * FROM notifications
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid;

-- name: ListNotificationsCursor :many
SELECT * FROM notifications
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- A separate unread query (instead of a "(NOT @unread_only OR read_at IS
-- NULL)" flag) lets a generic plan prove the partial index predicate and use
-- idx_notifications_tenant_user_unread.
-- name: ListUnreadNotificationsCursor :many
SELECT * FROM notifications
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND read_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: CountUnreadNotifications :one
SELECT count(*) FROM notifications
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND read_at IS NULL;

-- name: MarkNotificationRead :one
UPDATE notifications
SET read_at = COALESCE(read_at, NOW())
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
RETURNING *;

-- name: MarkNotificationUnread :one
UPDATE notifications
SET read_at = NULL
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
RETURNING *;

-- Bounded so one request cannot lock an arbitrarily large unread set. The
-- repository repeats the statement until a short batch is returned.
-- name: MarkAllNotificationsRead :execrows
WITH batch AS (
    SELECT id
    FROM notifications
    WHERE tenant_id = @tenant_id::uuid
      AND user_id = @user_id::uuid
      AND read_at IS NULL
    ORDER BY created_at, id
    LIMIT @batch_size::int
)
UPDATE notifications AS n
SET read_at = NOW()
FROM batch
WHERE n.id = batch.id;

-- name: GetNotificationPreference :one
SELECT * FROM notification_preferences
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND type = @type::text
  AND channel = @channel::text;

-- name: ListNotificationPreferences :many
SELECT * FROM notification_preferences
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
ORDER BY type ASC, channel ASC;

-- name: UpsertNotificationPreference :one
INSERT INTO notification_preferences (tenant_id, user_id, type, channel, enabled)
VALUES (@tenant_id::uuid, @user_id::uuid, @type::text, @channel::text, @enabled::boolean)
ON CONFLICT (tenant_id, user_id, type, channel)
DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()
RETURNING *;
