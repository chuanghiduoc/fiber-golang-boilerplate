// Package webhooks owns outbound webhook endpoints/deliveries, inbound
// sources/events, the public HMAC receiver, and the durable delivery worker.
package webhooks

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhooksprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/privacy"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// Module registers the webhooks HTTP surface.
type Module struct {
	handler *webhookshandler.Handler

	// Set by Build; a module built with New (tests) has no jobs or
	// background work.
	service        webhookservice.Service
	dispatcher     *webhookservice.Dispatcher
	deliveryWorker *webhookservice.DeliveryWorker
	client         *webhook.Client
	retentionDays  int32
	privacy        *webhooksprivacy.Provider
	tenantPurger   tenancycontracts.Purger
}

var _ app.PrivacyProvider = (*Module)(nil)

// New returns the webhooks module.
func New(handler *webhookshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return webhookscatalog.Owner }

// RegisterPrivacy implements app.PrivacyProvider: stored event history can
// name a user even though endpoint configuration is tenant-wide.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "webhooks", Exporter: m.privacy, Eraser: m.privacy})
}

// RegisterRoutes mounts tenant-scoped administration and the public receiver.
// Secret-returning routes (endpoint/source create and rotate-secret)
// deliberately omit idempotency caching so a plaintext secret is never
// persisted in the shared response cache; a retried rotate rotates again and
// returns the newer secret.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	rl := deps.Limiters
	auth := deps.Authenticate()
	// Administration mutations are Tier 2: a required intent row is written
	// before the handler runs (after idempotency where present), so an
	// unavailable audit store rejects the change with 503. The service's
	// domain event is recorded as a child of that intent.
	guard := func(action, resource string) fiber.Handler {
		return audithttp.Guard(deps.Audit, audithttp.Policy{
			Mode:     audithttp.ModeRequired,
			Action:   action,
			Resource: resource,
		})
	}
	read := authzhttp.Require(webhookscatalog.PermissionWebhooksRead)
	write := authzhttp.Require(webhookscatalog.PermissionWebhooksWrite)

	// The /webhooks prefix is shared by protected administration and the
	// public receiver. Mount auth per route rather than on the group so the
	// receiver is not forced through authentication.
	admin := v1.Group("/webhooks")
	admin.Get("/endpoints", auth, rl.Normal, read, m.handler.ListEndpoints)
	admin.Post("/endpoints", auth, rl.Normal, write, guard("webhook.endpoint.created", "webhook_endpoint"), m.handler.CreateEndpoint)
	admin.Get("/endpoints/:id", auth, rl.Normal, read, m.handler.GetEndpoint)
	admin.Put("/endpoints/:id", auth, rl.Normal, write, deps.Idempotency, guard("webhook.endpoint.updated", "webhook_endpoint"), m.handler.UpdateEndpoint)
	admin.Delete("/endpoints/:id", auth, rl.Normal, write, deps.Idempotency, guard("webhook.endpoint.deleted", "webhook_endpoint"), m.handler.DeleteEndpoint)
	admin.Post("/endpoints/:id/rotate-secret", auth, rl.Normal, write, guard("webhook.endpoint.secret_rotated", "webhook_endpoint"), m.handler.RotateEndpointSecret)

	admin.Get("/deliveries", auth, rl.Normal, read, m.handler.ListDeliveries)
	admin.Get("/deliveries/:id", auth, rl.Normal, read, m.handler.GetDelivery)
	admin.Get("/deliveries/:id/attempts", auth, rl.Normal, read, m.handler.ListDeliveryAttempts)
	admin.Post("/deliveries/:id/replay", auth, rl.Normal, write, deps.Idempotency, guard("webhook.delivery.replayed", "webhook_delivery"), m.handler.ReplayDelivery)

	admin.Get("/inbound-sources", auth, rl.Normal, read, m.handler.ListInboundSources)
	admin.Post("/inbound-sources", auth, rl.Normal, write, guard("webhook.inbound_source.created", "webhook_inbound_source"), m.handler.CreateInboundSource)
	admin.Get("/inbound-sources/:id", auth, rl.Normal, read, m.handler.GetInboundSource)
	admin.Put("/inbound-sources/:id", auth, rl.Normal, write, deps.Idempotency, guard("webhook.inbound_source.updated", "webhook_inbound_source"), m.handler.UpdateInboundSource)
	admin.Delete("/inbound-sources/:id", auth, rl.Normal, write, deps.Idempotency, guard("webhook.inbound_source.deleted", "webhook_inbound_source"), m.handler.DeleteInboundSource)
	admin.Post("/inbound-sources/:id/rotate-secret", auth, rl.Normal, write, guard("webhook.inbound_source.secret_rotated", "webhook_inbound_source"), m.handler.RotateInboundSourceSecret)
	admin.Get("/inbound-sources/:id/events", auth, rl.Normal, read, m.handler.ListInboundEvents)

	// Public receiver: no JWT and no tenant header. The source ID resolves the
	// tenant and the source secret authenticates the caller.
	v1.Post("/webhooks/inbound/:id", rl.Strict, m.handler.ReceiveInbound)
}
