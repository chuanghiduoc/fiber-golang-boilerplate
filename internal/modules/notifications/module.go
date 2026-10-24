// Package notifications owns the in-app notification feed, channel
// preferences, and durable email delivery for notifications.
package notifications

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module owns the authenticated notification and preference routes.
type Module struct {
	handler *notificationshandler.Handler
}

func New(handler *notificationshandler.Handler) *Module {
	return &Module{handler: handler}
}

func (m *Module) Name() string { return "notifications" }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	rl := deps.Limiters

	notifications := v1.Group("/notifications", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions))
	notifications.Get("/unread-count", rl.Relaxed, authz.Require(authz.PermissionNotificationsRead), m.handler.UnreadCount)
	notifications.Get("/", rl.Relaxed, authz.Require(authz.PermissionNotificationsRead), m.handler.List)
	notifications.Get("/:id", rl.Relaxed, authz.Require(authz.PermissionNotificationsRead), m.handler.Get)
	notifications.Post("/read-all", rl.Normal, authz.Require(authz.PermissionNotificationsWrite), deps.Idempotency, m.handler.MarkAllRead)
	notifications.Post("/:id/read", rl.Normal, authz.Require(authz.PermissionNotificationsWrite), deps.Idempotency, m.handler.MarkRead)
	notifications.Post("/send", rl.Normal, authz.Require(authz.PermissionNotificationsSend), deps.Idempotency, m.handler.Send)

	preferences := v1.Group("/notification-preferences", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions))
	preferences.Get("/", rl.Relaxed, authz.Require(authz.PermissionNotificationsRead), m.handler.ListPreferences)
	preferences.Put("/", rl.Normal, authz.Require(authz.PermissionNotificationsWrite), deps.Idempotency, m.handler.SetPreference)
}
