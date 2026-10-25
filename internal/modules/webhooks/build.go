package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// retentionInterval is how often delivered/attempt rows past
// WEBHOOK_RETENTION_DAYS are pruned.
const retentionInterval = time.Hour

// Provides lists what webhooks supplies to other modules.
type Provides struct {
	// Dispatcher publishes domain events (app.Deps.Webhooks); identity and
	// files emit through it without importing this module.
	Dispatcher webhookcontracts.Dispatcher
}

// Settings are the configuration webhooks owns. The composition root resolves
// the secret keys (including the local-only JWT_SECRET fallback), so the
// module never reads another section.
type Settings struct {
	// Webhook holds delivery, retention, inbound, and SSRF settings.
	Webhook config.WebhookConfig
	// SecretKey seals endpoint and inbound-source secrets.
	SecretKey string
	// PreviousSecretKeys open secrets sealed before a key rotation.
	PreviousSecretKeys []string
}

// Build constructs tenant-scoped endpoints, durable outbound deliveries, and
// the public inbound receiver. Secrets are sealed with the dedicated webhook
// key; the HTTP client pins DNS results to vetted public IPs and never
// follows redirects. tenantActive is identity's tenant check, so the inbound
// receiver rejects suspended tenants like every request.
func Build(infra app.Infra, settings Settings, tenantActive func(ctx context.Context, tenantID int64) (bool, error)) (*Module, Provides, error) {
	cfg := settings.Webhook
	codec, err := webhookservice.NewSecretCodec(settings.SecretKey, settings.PreviousSecretKeys...)
	if err != nil {
		return nil, Provides{}, fmt.Errorf("failed to initialize webhook secret encryption: %w", err)
	}
	guard, err := webhook.NewGuard(cfg.SSRFAllowlistEntries())
	if err != nil {
		return nil, Provides{}, fmt.Errorf("failed to initialize webhook SSRF guard: %w", err)
	}
	client := webhook.NewClient(webhook.ClientConfig{
		Guard:          guard,
		Timeout:        cfg.RequestTimeout(),
		ConnectTimeout: cfg.ConnectTimeout(),
	})
	repo := webhookrepo.New(infra.Pool, infra.Queue)
	var tenants webhookservice.TenantChecker
	if tenantActive != nil {
		tenants = webhookservice.TenantCheckerFunc(tenantActive)
	}
	svc := webhookservice.New(repo, repo, repo, codec, infra.Audit, webhookservice.Options{
		DefaultMaxAttempts: cfg.MaxAttempts,
		DefaultTimeoutMS:   int32(cfg.RequestTimeoutSeconds * 1000), // #nosec G115 -- validated 1..120 seconds
		TimestampTolerance: cfg.TimestampTolerance(),
		Tenants:            tenants,
	})
	dispatcher := webhookservice.NewDispatcher(repo, infra.Queue)

	m := New(webhookshandler.New(svc))
	m.service = svc
	m.dispatcher = dispatcher
	m.client = client
	m.deliveryWorker = webhookservice.NewDeliveryWorker(repo, codec, client, webhookservice.WorkerOptions{
		RetryBase: cfg.RetryBase(),
		RetryMax:  cfg.RetryMax(),
	})
	m.retentionDays = int32(cfg.RetentionDays) // #nosec G115 -- validated 1..3650 days
	return m, Provides{Dispatcher: dispatcher}, nil
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return webhookscatalog.Permissions() }

// RegisterJobs implements app.JobProvider: the delivery worker sends signed
// requests and the dispatch job fans a committed domain event out to
// endpoints. A module built with New (tests) registers nothing.
func (m *Module) RegisterJobs(registrar jobs.Registrar) {
	if m.deliveryWorker != nil {
		registrar.Register(webhookservice.DeliveryJobKind, m.deliveryWorker.Handle)
	}
	if m.dispatcher != nil {
		registrar.Register(webhookservice.DispatchJobKind, m.dispatcher.HandleDispatchJob)
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
