// Package notifications owns the in-app notification feed, channel
// preferences, and durable email delivery for notifications.
package notifications

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
)

// Module owns the authenticated notification and preference routes.
type Module struct {
	handler *notificationshandler.Handler
}

func New(handler *notificationshandler.Handler) *Module {
	return &Module{handler: handler}
}

func (m *Module) Name() string { return notificationscatalog.Owner }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	rl := deps.Limiters
	auth := deps.Authenticate()
	// The admin send is a privileged cross-user write, so it is Tier 2: a
	// required intent row is written after idempotency and before the
	// handler. The caller's own feed/preference writes stay fallback-audited.
	sendGuard := audithttp.Guard(deps.Audit, audithttp.Policy{
		Mode:     audithttp.ModeRequired,
		Action:   "notification.sent",
		Resource: "notification",
	})

	notifications := v1.Group("/notifications", auth)
	notifications.Get("/unread-count", rl.Relaxed, authzhttp.Require(notificationscatalog.PermissionNotificationsRead), m.handler.UnreadCount)
	notifications.Get("/", rl.Relaxed, authzhttp.Require(notificationscatalog.PermissionNotificationsRead), m.handler.List)
	notifications.Get("/:id", rl.Relaxed, authzhttp.Require(notificationscatalog.PermissionNotificationsRead), m.handler.Get)
	notifications.Post("/read-all", rl.Normal, authzhttp.Require(notificationscatalog.PermissionNotificationsWrite), deps.Idempotency, m.handler.MarkAllRead)
	notifications.Post("/:id/read", rl.Normal, authzhttp.Require(notificationscatalog.PermissionNotificationsWrite), deps.Idempotency, m.handler.MarkRead)
	notifications.Post("/send", rl.Normal, authzhttp.Require(notificationscatalog.PermissionNotificationsSend), deps.Idempotency, sendGuard, m.handler.Send)

	preferences := v1.Group("/notification-preferences", auth)
	preferences.Get("/", rl.Relaxed, authzhttp.Require(notificationscatalog.PermissionNotificationsRead), m.handler.ListPreferences)
	preferences.Put("/", rl.Normal, authzhttp.Require(notificationscatalog.PermissionNotificationsWrite), deps.Idempotency, m.handler.SetPreference)
}
