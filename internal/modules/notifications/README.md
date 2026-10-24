# Notifications module

Owns the per-user in-app notification feed, per-type channel preferences, and
the durable email channel.

## Routes

| Method | Path | Permission | Description |
| --- | --- | --- | --- |
| GET | `/api/v1/notifications/` | `notifications:read` | Cursor list (`limit`, `startingAfter`, `unreadOnly`) |
| GET | `/api/v1/notifications/unread-count` | `notifications:read` | Unread badge count |
| GET | `/api/v1/notifications/:id` | `notifications:read` | One owned notification |
| POST | `/api/v1/notifications/:id/read` | `notifications:write` | Idempotent mark-read |
| POST | `/api/v1/notifications/read-all` | `notifications:write` | Mark all unread rows |
| POST | `/api/v1/notifications/send` | `notifications:send` | Admin send to a user in the tenant |
| GET | `/api/v1/notification-preferences/` | `notifications:read` | Explicit preferences |
| PUT | `/api/v1/notification-preferences/` | `notifications:write` | Upsert `type` + `in_app`/`email` |

All routes require JWT and are scoped to both the request tenant and the
authenticated user. Cross-tenant and other-user ids return `404`. Mutations use
`Idempotency-Key`; the generic audit middleware records successful writes.

## Data

`notifications` stores the in-app delivery (`type`, `title`, `body`, JSON
`data`, `read_at`). `notification_preferences` stores explicit
`(tenant_id, user_id, type, channel)` opt-outs; a missing row means the channel
is enabled. `PUT /notification-preferences` requires an explicit boolean
`enabled` (an omitted field is a `422`, never an implicit disable). Title,
body, and `data` must be valid UTF-8 without NUL characters (including a
`\u0000` JSON escape), otherwise `400`. Both tables are tenant-owned and
cascade with the user. The feed list uses separate all/unread queries so the
unread filter can use the partial `idx_notifications_tenant_user_unread`.

## Cross-module API

Other modules import `internal/contracts/notifications`:

- `notifications.Notifier.Notify(ctx, CreateNotificationInput)` creates a
  notification.
- `identity.NotificationRecipientLookup` (`internal/contracts/identity`) is implemented by the identity
  adapter; notifications never read the `users` table.

When email is enabled, the service resolves the recipient, inserts the in-app
row, and enqueues `email.send` (or `email.send.encrypted`) in the same
transaction. An enqueue failure rolls the row back. Suppression by both
channels is a successful no-op. Delivery is at-least-once.

## Tests

Unit tests cover service, handler, tenant/user isolation, preferences, email
payload, suppression, and queue rollback. Integration tests
(`-tags=integration`, `TEST_DATABASE_URL` or Testcontainers) cover repository
scoping, cursor/unread/read, preferences, job payload, and queue failure.
