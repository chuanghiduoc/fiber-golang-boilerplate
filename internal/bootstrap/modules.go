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
	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
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
	Webhooks       webhookcontracts.Dispatcher

	// Startup runs one-time checks that need the database (tenant
	// existence, admin seed) inside the caller's startup deadline.
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
	identityModule, identityProvides, err := identity.Build(infra, identity.Settings{
		OAuth: cfg.OAuth,
		Admin: cfg.Admin,
	})
	if err != nil {
		return nil, err
	}
	webhooksModule, webhooksProvides, err := webhooks.Build(infra, webhooks.Settings{
		Webhook: cfg.Webhook,
		// JWT_SECRET is the local/test-only fallback; production requires
		// the dedicated key (config validation).
		SecretKey:          cfg.Webhook.ResolvedSecretKey(cfg.JWT.Secret),
		PreviousSecretKeys: cfg.Webhook.PreviousSecretKeyList(),
	}, identityProvides.TenantActive)
	if err != nil {
		return nil, err
	}
	// User registration enqueues webhook events inside its own transaction;
	// identity is built before webhooks (which needs TenantActive), so the
	// dispatcher is attached afterwards. Files takes it at construction.
	identityModule.AttachWebhooks(webhooksProvides.Dispatcher)
	filesModule, filesProvides := files.Build(infra, files.Settings{
		MaxFileSize:  cfg.Storage.MaxFileSize,
		AllowedTypes: cfg.Storage.AllowedTypes(),
	}, webhooksProvides.Dispatcher)

	apikeysModule, apikeysProvides := apikeys.Build(infra, identityProvides.Principals)

	// cmd/gen (make gen module=<name>) inserts generated modules above the
	// two gen: markers below; keep both lines.
	// gen:module-build

	return &Modules{
		List: []app.Module{
			identityModule,
			filesModule,
			admin.Build(identityProvides.Admin, filesProvides.Admin),
			features.Build(infra),
			webhooksModule,
			apikeysModule,
			notifications.Build(infra, identityProvides.Recipients),
			// gen:module-list
		},
		Sessions:       identityProvides.Sessions,
		TenantResolver: identityProvides.TenantResolver,
		APIKeyVerifier: apikeysProvides.Verifier,
		Webhooks:       webhooksProvides.Dispatcher,
		Startup:        identityModule.Startup,
		Background:     []func(context.Context){webhooksModule.RunRetention},
		Close:          []func(){webhooksModule.Close},
	}, nil
}
