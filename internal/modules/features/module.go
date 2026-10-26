// Package features owns feature-flag administration routes. Evaluation and
// the compile-time catalog live in internal/platform/features; this module is
// the tenant-scoped back-office surface for database defaults, kill switches,
// and tenant/user targets.
package features

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
)

// Module wires the feature-flag admin HTTP surface into the application.
type Module struct {
	handler      *featureshandler.Handler
	privacy      *featuresprivacy.Provider
	tenantPurger tenancycontracts.Purger
}

var _ app.PrivacyProvider = (*Module)(nil)

// New builds the module from its handler.
func New(handler *featureshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return featurescatalog.Owner }

// RegisterPrivacy implements app.PrivacyProvider.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "features", Exporter: m.privacy, Eraser: m.privacy})
}

// RegisterRoutes mounts the admin feature-flag routes below /api/v1.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	rl := deps.Limiters
	// Flag mutations are Tier 2: a required intent row is written before the
	// handler runs, so an unavailable audit store rejects the change (503).
	guard := func(action, resource string) fiber.Handler {
		return audithttp.Guard(deps.Audit, audithttp.Policy{
			Mode:     audithttp.ModeRequired,
			Action:   action,
			Resource: resource,
		})
	}

	admin := v1.Group("/admin/features", deps.Authenticate())
	admin.Get("/flags", rl.Relaxed, authzhttp.Require(featurescatalog.PermissionAdminFeaturesRead), m.handler.ListFlags)
	admin.Get("/flags/:key", rl.Relaxed, authzhttp.Require(featurescatalog.PermissionAdminFeaturesRead), m.handler.GetFlag)
	// Process-wide rows also accept a platform admin; the service re-checks
	// the platform permission. The route requires a second factor because one
	// of its accepted permissions is a platform grant: MFA applies to the
	// whole route (a tenant admin using the tenant permission must also have
	// one), which is stricter than splitting the path and simpler to audit.
	mfa := authzhttp.RequireMFA(deps.Config.MFA.EffectiveMaxAge())
	flagWrite := authzhttp.RequireAny(featurescatalog.PermissionAdminFeaturesWrite, authz.PermissionPlatformFeaturesWrite)
	admin.Put("/flags/:key", rl.Normal, mfa, flagWrite, deps.Idempotency, guard("admin.feature_flag.updated", "feature_flag"), m.handler.UpsertFlag)
	admin.Delete("/flags/:key", rl.Normal, mfa, flagWrite, deps.Idempotency, guard("admin.feature_flag.deleted", "feature_flag"), m.handler.DeleteFlag)
	admin.Get("/flags/:key/targets", rl.Relaxed, authzhttp.Require(featurescatalog.PermissionAdminFeaturesRead), m.handler.ListTargets)
	admin.Put("/flags/:key/targets", rl.Normal, authzhttp.Require(featurescatalog.PermissionAdminFeaturesWrite), deps.Idempotency, guard("admin.feature_flag_target.updated", "feature_flag_target"), m.handler.UpsertTarget)
	admin.Delete("/flags/:key/targets/:id", rl.Normal, authzhttp.Require(featurescatalog.PermissionAdminFeaturesWrite), deps.Idempotency, guard("admin.feature_flag_target.deleted", "feature_flag_target"), m.handler.DeleteTarget)
}
