// Package webhooks owns outbound webhook endpoints/deliveries, inbound
// sources/events, the public HMAC receiver, and the durable delivery worker.
package webhooks

import (
	"io/fs"
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhooksprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/privacy"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// webhooksTag groups the module's operations in the OpenAPI document.
const webhooksTag = "Webhooks"

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

var (
	_ app.Module          = (*Module)(nil)
	_ app.RouteProvider   = (*Module)(nil)
	_ app.PrivacyProvider = (*Module)(nil)
)

// New returns the webhooks module.
func New(handler *webhookshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return webhookscatalog.Owner }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) Errors() []apperror.Spec { return webhookscatalog.Errors() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return webhookscatalog.Locales() }

// RegisterPrivacy implements app.PrivacyProvider: stored event history can
// name a user even though endpoint configuration is tenant-wide.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "webhooks", Exporter: m.privacy, Eraser: m.privacy})
}

// guard builds the required Tier 2 intent policy for a mutation.
func guard(action, resource string) httpapi.AuditPolicy {
	return httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{
		Action:   action,
		Resource: resource,
		Mode:     audithttp.ModeRequired,
	}}
}

// RegisterRoutes mounts the tenant-scoped administration and the public
// receiver through the huma registration path. Secret-returning operations are
// never idempotency-cached, so a plaintext secret never sits in the shared
// response cache; a retried rotation rotates again and returns the newest
// secret.
func (m *Module) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	read := httpapi.Policy{
		Permission: webhookscatalog.PermissionWebhooksRead,
		RateLimit:  httpapi.RateLimitNormal,
	}
	write := func(audit httpapi.AuditPolicy) httpapi.Policy {
		return httpapi.Policy{
			Permission: webhookscatalog.PermissionWebhooksWrite,
			RateLimit:  httpapi.RateLimitNormal,
			Audit:      audit,
		}
	}
	op := func(method, path, operationID, summary string) httpapi.Op {
		return httpapi.Op{
			Method:      method,
			Path:        httpapi.APIPrefix + path,
			OperationID: operationID,
			Tags:        []string{webhooksTag},
			Summary:     summary,
		}
	}

	httpapi.Register(api, op(http.MethodGet, "/webhooks/endpoints", "listWebhookEndpoints",
		"List webhook endpoints"), read, m.handler.ListEndpoints)

	createEndpoint := write(guard("webhook.endpoint.created", "webhook_endpoint"))
	createEndpoint.NoStore = true
	httpapi.Register(api, op(http.MethodPost, "/webhooks/endpoints", "createWebhookEndpoint",
		"Create a webhook endpoint"), createEndpoint, m.handler.CreateEndpoint)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/endpoints/{endpointId}", "getWebhookEndpoint",
		"Get a webhook endpoint"), read, m.handler.GetEndpoint)

	updateEndpoint := write(guard("webhook.endpoint.updated", "webhook_endpoint"))
	updateEndpoint.Idempotent = true
	httpapi.Register(api, op(http.MethodPatch, "/webhooks/endpoints/{endpointId}", "updateWebhookEndpoint",
		"Update a webhook endpoint"), updateEndpoint, m.handler.UpdateEndpoint)

	deleteEndpoint := write(guard("webhook.endpoint.deleted", "webhook_endpoint"))
	deleteEndpoint.Idempotent = true
	httpapi.Register(api, op(http.MethodDelete, "/webhooks/endpoints/{endpointId}", "deleteWebhookEndpoint",
		"Delete a webhook endpoint"), deleteEndpoint, m.handler.DeleteEndpoint)

	rotateEndpoint := write(guard("webhook.endpoint.secret_rotated", "webhook_endpoint"))
	rotateEndpoint.NoStore = true
	httpapi.Register(api, op(http.MethodPost, "/webhooks/endpoints/{endpointId}/rotate-secret", "rotateWebhookEndpointSecret",
		"Rotate a webhook endpoint signing secret"), rotateEndpoint, m.handler.RotateEndpointSecret)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/deliveries", "listWebhookDeliveries",
		"List webhook deliveries"), read, m.handler.ListDeliveries)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/deliveries/{deliveryId}", "getWebhookDelivery",
		"Get a webhook delivery"), read, m.handler.GetDelivery)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/deliveries/{deliveryId}/attempts", "listWebhookDeliveryAttempts",
		"List webhook delivery attempts"), read, m.handler.ListDeliveryAttempts)

	replay := write(guard("webhook.delivery.replayed", "webhook_delivery"))
	replay.Idempotent = true
	httpapi.Register(api, op(http.MethodPost, "/webhooks/deliveries/{deliveryId}/replay", "replayWebhookDelivery",
		"Replay a webhook delivery"), replay, m.handler.ReplayDelivery)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/inbound-sources", "listInboundSources",
		"List inbound webhook sources"), read, m.handler.ListInboundSources)

	createSource := write(guard("webhook.inbound_source.created", "webhook_inbound_source"))
	createSource.NoStore = true
	httpapi.Register(api, op(http.MethodPost, "/webhooks/inbound-sources", "createInboundSource",
		"Create an inbound webhook source"), createSource, m.handler.CreateInboundSource)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/inbound-sources/{sourceId}", "getInboundSource",
		"Get an inbound webhook source"), read, m.handler.GetInboundSource)

	updateSource := write(guard("webhook.inbound_source.updated", "webhook_inbound_source"))
	updateSource.Idempotent = true
	httpapi.Register(api, op(http.MethodPatch, "/webhooks/inbound-sources/{sourceId}", "updateInboundSource",
		"Update an inbound webhook source"), updateSource, m.handler.UpdateInboundSource)

	deleteSource := write(guard("webhook.inbound_source.deleted", "webhook_inbound_source"))
	deleteSource.Idempotent = true
	httpapi.Register(api, op(http.MethodDelete, "/webhooks/inbound-sources/{sourceId}", "deleteInboundSource",
		"Delete an inbound webhook source"), deleteSource, m.handler.DeleteInboundSource)

	rotateSource := write(guard("webhook.inbound_source.secret_rotated", "webhook_inbound_source"))
	rotateSource.NoStore = true
	httpapi.Register(api, op(http.MethodPost, "/webhooks/inbound-sources/{sourceId}/rotate-secret", "rotateInboundSourceSecret",
		"Rotate an inbound source verification secret"), rotateSource, m.handler.RotateInboundSourceSecret)

	httpapi.Register(api, op(http.MethodGet, "/webhooks/inbound-sources/{sourceId}/events", "listInboundSourceEvents",
		"List inbound webhook events"), read, m.handler.ListInboundSourceEvents)

	// Public receiver: no JWT and no tenant header. The source ID resolves the
	// tenant and the source secret authenticates the caller. The service writes
	// the Tier 1 webhook.received row with the source tenant in the same
	// transaction as the event, because no tenant exists before the handler.
	receive := op(http.MethodPost, "/webhooks/inbound/{sourceId}", "receiveInboundWebhook",
		"Receive an inbound webhook")
	receive.Errors = []int{http.StatusUnauthorized}
	httpapi.Register(api, receive, httpapi.Policy{
		Public:    true,
		RateLimit: httpapi.RateLimitStrict,
		Audit:     httpapi.AuditPolicy{Tier1Action: "webhook.received"},
	}, m.handler.ReceiveInbound)
}
