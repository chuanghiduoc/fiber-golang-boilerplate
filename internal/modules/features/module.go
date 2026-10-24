// Package features owns feature-flag administration routes. Evaluation and
// the compile-time catalog live in internal/platform/features; this module is
// the tenant-scoped back-office surface for database defaults, kill switches,
// and tenant/user targets.
package features

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module wires the feature-flag admin HTTP surface into the application.
type Module struct {
	handler *featureshandler.Handler
}

// New builds the module from its handler.
func New(handler *featureshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return "features" }

// RegisterRoutes mounts the admin feature-flag routes below /api/v1.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	rl := deps.Limiters

	admin := v1.Group("/admin/features", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions))
	admin.Get("/flags", rl.Relaxed, authz.Require(authz.PermissionAdminFeaturesRead), m.handler.ListFlags)
	admin.Get("/flags/:key", rl.Relaxed, authz.Require(authz.PermissionAdminFeaturesRead), m.handler.GetFlag)
	admin.Put("/flags/:key", rl.Normal, authz.Require(authz.PermissionAdminFeaturesWrite), deps.Idempotency, m.handler.UpsertFlag)
	admin.Delete("/flags/:key", rl.Normal, authz.Require(authz.PermissionAdminFeaturesWrite), deps.Idempotency, m.handler.DeleteFlag)
	admin.Get("/flags/:key/targets", rl.Relaxed, authz.Require(authz.PermissionAdminFeaturesRead), m.handler.ListTargets)
	admin.Put("/flags/:key/targets", rl.Normal, authz.Require(authz.PermissionAdminFeaturesWrite), deps.Idempotency, m.handler.UpsertTarget)
	admin.Delete("/flags/:key/targets/:id", rl.Normal, authz.Require(authz.PermissionAdminFeaturesWrite), deps.Idempotency, m.handler.DeleteTarget)
}
