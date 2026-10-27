// Package bootstrap holds the single ordered list of business modules and
// their contract cross-wiring. cmd/api uses it to build the application and
// test/arch uses the same list, so every new module is automatically covered
// by the route, authorization, and ownership checks.
//
// This is the only non-cmd package allowed to import several modules (it is
// part of the composition root, like cmd/api). Modules never import it.
package bootstrap

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// Modules is the built module set plus the platform-facing values the
// composition root installs in app.Deps and runs during the lifecycle.
type Modules struct {
	// List is registration order: routes, catalogs, and providers are
	// registered in this order.
	List []app.Module

	Sessions       session.Store
	TenantResolver tenant.Resolver
	APIKeyVerifier *apikey.Verifier
	// Privacy is the registry modules register their exporters/erasers into.
	Privacy *privacycontracts.Registry
	// Tenancy is the registry modules register their tenant-wide purgers into.
	Tenancy *tenancycontracts.Registry

	// Startup runs one-time checks that need the database (tenant existence,
	// permission catalog sync) inside the caller's startup deadline.
	Startup func(ctx context.Context) error
	// Background runs best-effort loops that share the job worker lifecycle
	// (canceled with the worker context during shutdown).
	Background []func(ctx context.Context)
	// Close releases module resources after HTTP and workers stopped.
	Close []func()
}

// Build constructs every module from infra in dependency order and wires the
// cross-module contracts. It performs no I/O. cfg supplies each module's own
// configuration section, passed explicitly so no module reads another's. To
// add a module: build it here (passing only contracts from earlier modules
// and its own settings) and append it to List.
func Build(cfg *config.Config, infra app.Infra) (*Modules, error) {
	privacyRegistry := privacycontracts.NewRegistry()
	tenancyRegistry := tenancycontracts.NewRegistry()
	identityModule, err := identity.Build(infra, identity.Settings{
		OAuth:          cfg.OAuth,
		DataEncryption: cfg.DataEncryption,
		WebAuthn:       cfg.WebAuthn,
	}, identity.Deps{Privacy: privacyRegistry, Tenancy: tenancyRegistry})
	if err != nil {
		return nil, err
	}
	requireWebhookHTTPS, err := cfg.Webhook.RequiresHTTPS(cfg.App.Env)
	if err != nil {
		return nil, err
	}
	webhooksModule, err := webhooks.Build(infra, webhooks.Settings{
		Webhook: cfg.Webhook,
		// The dedicated key is required in every environment; config
		// validation rejects an unset key and the published samples.
		SecretKey:          cfg.Webhook.ResolvedSecretKey(),
		PreviousSecretKeys: cfg.Webhook.PreviousSecretKeyList(),
		RequireHTTPS:       requireWebhookHTTPS,
	}, webhooks.Deps{TenantActive: identityModule.TenantActive()})
	if err != nil {
		return nil, err
	}
	// Identity and files publish domain events on infra.Events inside their
	// transactions; webhooks subscribes to them (app.EventProvider), so no
	// producer is wired to the webhooks module.
	filesModule, err := files.Build(infra, files.Settings{
		MaxFileSize:        cfg.Storage.MaxFileSize,
		AllowedTypes:       cfg.Storage.AllowedTypes(),
		TenantQuotaBytes:   cfg.Files.TenantQuotaBytes,
		UserQuotaBytes:     cfg.Files.UserQuotaBytes,
		ScanDriver:         cfg.Files.EnabledScanDriver(),
		ClamAVAddr:         cfg.Files.ClamAVAddr,
		ClamAVTimeout:      cfg.Files.ClamAVTimeout,
		TrashRetentionDays: cfg.Retention.FilesTrashRetentionDays(),
	}, files.Deps{})
	if err != nil {
		return nil, err
	}

	apikeysModule, err := apikeys.Build(infra, apikeys.Settings{
		MaxLifetime: cfg.APIKeys.MaxLifetime,
	}, apikeys.Deps{Principals: identityModule.Principals(), MemberRanks: identityModule.MemberRanks()})
	if err != nil {
		return nil, err
	}

	// cmd/gen (make gen module=<name>) inserts generated modules above the
	// two gen: markers below; keep both lines.
	// gen:module-build

	notificationsModule, err := notifications.Build(infra, notifications.Settings{
		RetentionDays: cfg.Retention.NotificationsRetentionDays(),
	}, notifications.Deps{Recipients: identityModule.Recipients()})
	if err != nil {
		return nil, err
	}

	// Each module contributes its tenant-wide purgers through
	// app.TenantPurgeProvider. The composition root owns the order: non-identity
	// first and identity last, because only identity deletes memberships, so
	// every other module can still reach the rows its foreign keys reference.
	featuresModule, err := features.Build(infra, features.Settings{}, features.Deps{})
	if err != nil {
		return nil, err
	}
	registerTenantPurgers(tenancyRegistry,
		filesModule,
		apikeysModule,
		featuresModule,
		webhooksModule,
		notificationsModule,
		// cmd/gen (make gen module=<name>) inserts generated modules above this
		// marker; keep the line.
		// gen:module-purge
	)
	registerTenantPurgers(tenancyRegistry, identityModule)
	// The durable jobs table is platform-owned rather than part of a module,
	// so its purger is registered here, after every module: queued encrypted
	// email payloads must not survive the tenant that produced them, and
	// running jobs are left to finish.
	tenancyRegistry.Register(tenancycontracts.Provider{Name: "jobs", Purger: jobs.NewTenantPurger(infra.DB)})

	adminModule, err := admin.Build(infra, admin.Settings{}, admin.Deps{})
	if err != nil {
		return nil, err
	}

	return &Modules{
		List: []app.Module{
			identityModule,
			filesModule,
			adminModule,
			featuresModule,
			webhooksModule,
			apikeysModule,
			notificationsModule,
			// gen:module-list
		},
		Sessions:       identityModule.SessionStore(),
		TenantResolver: identityModule.TenantResolver(),
		APIKeyVerifier: apikeysModule.Verifier(),
		Privacy:        privacyRegistry,
		Tenancy:        tenancyRegistry,
		Startup:        identityModule.Startup,
		Background:     []func(context.Context){webhooksModule.RunRetention},
		Close:          []func(){webhooksModule.Close},
	}, nil
}

// registerTenantPurgers registers every purger the given modules expose, in
// argument order. Modules that have no tenant-owned data contribute nothing.
func registerTenantPurgers(registry *tenancycontracts.Registry, modules ...app.Module) {
	for _, module := range modules {
		provider, ok := module.(app.TenantPurgeProvider)
		if !ok {
			continue
		}
		for _, purger := range provider.TenantPurgers() {
			registry.Register(purger)
		}
	}
}
