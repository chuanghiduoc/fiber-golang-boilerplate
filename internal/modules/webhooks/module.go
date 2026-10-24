// Package webhooks owns outbound webhook endpoints/deliveries, inbound
// sources/events, the public HMAC receiver, and the durable delivery worker.
package webhooks

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module registers the webhooks HTTP surface.
type Module struct {
	handler *webhookshandler.Handler
}

// New returns the webhooks module.
func New(handler *webhookshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return "webhooks" }

// RegisterRoutes mounts tenant-scoped administration and the public receiver.
// Secret-returning creates deliberately omit idempotency caching so a secret is
// never persisted in the shared response cache. Rotation is replay-safe: a
// repeated Idempotency-Key returns the already-issued secret instead of
// rotating again.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	rl := deps.Limiters
	jwt := middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions)

	// The /webhooks prefix is shared by protected administration and the
	// public receiver. Mount auth per route rather than on the group so the
	// receiver is not forced through JWT.
	admin := v1.Group("/webhooks")
	admin.Get("/endpoints", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.ListEndpoints)
	admin.Post("/endpoints", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), m.handler.CreateEndpoint)
	admin.Get("/endpoints/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.GetEndpoint)
	admin.Put("/endpoints/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.UpdateEndpoint)
	admin.Delete("/endpoints/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.DeleteEndpoint)
	admin.Post("/endpoints/:id/rotate-secret", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.RotateEndpointSecret)

	admin.Get("/deliveries", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.ListDeliveries)
	admin.Get("/deliveries/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.GetDelivery)
	admin.Get("/deliveries/:id/attempts", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.ListDeliveryAttempts)
	admin.Post("/deliveries/:id/replay", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.ReplayDelivery)

	admin.Get("/inbound-sources", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.ListInboundSources)
	admin.Post("/inbound-sources", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), m.handler.CreateInboundSource)
	admin.Get("/inbound-sources/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.GetInboundSource)
	admin.Put("/inbound-sources/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.UpdateInboundSource)
	admin.Delete("/inbound-sources/:id", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.DeleteInboundSource)
	admin.Post("/inbound-sources/:id/rotate-secret", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksWrite), deps.Idempotency, m.handler.RotateInboundSourceSecret)
	admin.Get("/inbound-sources/:id/events", jwt, rl.Normal, authz.Require(authz.PermissionWebhooksRead), m.handler.ListInboundEvents)

	// Public receiver: no JWT and no tenant header. The source ID resolves the
	// tenant and the source secret authenticates the caller.
	v1.Post("/webhooks/inbound/:id", rl.Strict, m.handler.ReceiveInbound)
}
