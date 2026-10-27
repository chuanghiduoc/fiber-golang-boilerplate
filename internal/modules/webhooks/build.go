package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhooksprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/privacy"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const (
	// retentionInterval is how often delivered/attempt rows past
	// WEBHOOK_RETENTION are pruned.
	retentionInterval = time.Hour
	// eventJobMaxAttempts is the budget of each durable event subscription
	// job: fan-out only fails on database errors, which are usually
	// transient, so it matches the delivery job budget.
	eventJobMaxAttempts = 20
)

// Settings are the configuration webhooks owns. The composition root resolves
// the dedicated secret keys, so the module never reads another section.
type Settings struct {
	// Webhook holds delivery, retention, inbound, and SSRF settings.
	Webhook config.WebhookConfig
	// SecretKey seals endpoint and inbound-source secrets.
	SecretKey string
	// PreviousSecretKeys open secrets sealed before a key rotation.
	PreviousSecretKeys []string
	// RequireHTTPS rejects plaintext http:// endpoint URLs.
	RequireHTTPS bool
}

// Build constructs tenant-scoped endpoints, durable outbound deliveries, and
// the public inbound receiver. Secrets are sealed with the dedicated webhook
// key; the HTTP client pins DNS results to vetted public IPs and never
// follows redirects. tenantActive is identity's tenant check, so the inbound
// receiver rejects suspended tenants like every request.
// Deps are the contracts webhooks consumes from identity.
type Deps struct {
	TenantActive func(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

func Build(infra app.Infra, settings Settings, deps Deps) (*Module, error) {
	tenantActive := deps.TenantActive
	cfg := settings.Webhook
	codec, err := webhookservice.NewSecretCodec(settings.SecretKey, settings.PreviousSecretKeys...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize webhook secret encryption: %w", err)
	}
	ports, err := cfg.AllowedPortsList()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize webhook port policy: %w", err)
	}
	guard, err := webhook.NewGuardWithPorts(cfg.SSRFAllowlistEntries(), ports)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize webhook SSRF guard: %w", err)
	}
	client := webhook.NewClient(webhook.ClientConfig{
		Guard:          guard,
		Timeout:        cfg.RequestTimeout,
		ConnectTimeout: cfg.ConnectTimeout,
	})
	repo := webhookrepo.New(infra.DB, infra.Tx, infra.Queue, infra.Audit)
	var tenants webhookservice.TenantChecker
	if tenantActive != nil {
		tenants = webhookservice.TenantCheckerFunc(tenantActive)
	}
	svc := webhookservice.New(repo, repo, repo, codec, infra.Audit, webhookservice.Options{
		DefaultMaxAttempts: cfg.MaxAttempts,
		DefaultTimeoutMS:   int32(cfg.RequestTimeout.Milliseconds()), // #nosec G115 -- validated 1s..120s
		TimestampTolerance: cfg.TimestampTolerance,
		Tenants:            tenants,
		URLGuard:           guard,
		RequireHTTPS:       settings.RequireHTTPS,
	})
	dispatcher := webhookservice.NewDispatcher(repo)

	m := New(webhookshandler.New(svc))
	m.service = svc
	m.dispatcher = dispatcher
	m.client = client
	m.deliveryWorker = webhookservice.NewDeliveryWorker(repo, codec, client, webhookservice.WorkerOptions{
		RetryBase: cfg.RetryBase,
		RetryMax:  cfg.RetryMax,
	})
	m.retentionDays = int32(cfg.Retention / (24 * time.Hour)) // #nosec G115 -- validated 1..3650 whole days
	m.privacy = webhooksprivacy.New(webhookrepo.NewPrivacyStore(infra.DB))
	m.tenantPurger = webhookrepo.NewTenantPurger(infra.DB)
	return m, nil
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return webhookscatalog.Permissions() }

// RegisterJobs implements app.JobProvider: the delivery worker sends signed
// requests. A module built with New (tests) registers nothing.
func (m *Module) RegisterJobs(registrar jobs.Registrar) {
	if m.deliveryWorker != nil {
		registrar.Register(webhookservice.DeliveryJobKind, m.deliveryWorker.Handle)
	}
}

// RegisterEvents implements app.EventProvider: one durable subscription per
// webhook event type fans each committed event out to matching endpoints.
// Producers publish with events.Bus.PublishTx, so the subscription job
// commits with the business change. The types are constants, so a
// registration error is a programming error and fails startup loudly.
func (m *Module) RegisterEvents(bus *events.Bus) {
	if m.dispatcher == nil {
		return
	}
	for _, eventType := range webhookscatalog.SubscribedEvents() {
		if err := bus.SubscribeDurable(eventType, webhookscatalog.EventHandlerName, m.dispatcher.HandleEvent,
			events.WithMaxAttempts(eventJobMaxAttempts)); err != nil {
			panic(fmt.Sprintf("webhooks: subscribe to %s: %v", eventType, err))
		}
	}
}

// RunRetention prunes old deliveries/attempts until ctx is canceled. It is
// best-effort and shares the job worker's lifecycle.
func (m *Module) RunRetention(ctx context.Context) {
	if m.service == nil {
		return
	}
	webhookservice.RunRetention(ctx, m.service, m.retentionDays, retentionInterval)
}

// Close releases idle outbound connections at shutdown.
func (m *Module) Close() {
	if m.client != nil {
		m.client.CloseIdleConnections()
	}
}
