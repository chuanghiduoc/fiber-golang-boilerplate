package apikeys

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module owns tenant-scoped API-key administration. Keys authenticate through
// the platform api-key middleware on other modules; this module only manages
// their lifecycle.
type Module struct {
	handler *apikeyhandler.Handler
}

func New(handler *apikeyhandler.Handler) *Module {
	return &Module{handler: handler}
}

func (m *Module) Name() string { return "apikeys" }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	auth := apikey.Authenticate(middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions), deps.APIKeyVerifier)
	group := v1.Group("/admin/api-keys", auth, deps.Limiters.Normal)

	group.Get("/", authz.Require(authz.PermissionAdminAPIKeysRead), m.handler.List)
	group.Post("/", authz.Require(authz.PermissionAdminAPIKeysCreate), m.handler.Create)
	group.Get("/:id", authz.Require(authz.PermissionAdminAPIKeysRead), m.handler.Get)
	group.Put("/:id", authz.Require(authz.PermissionAdminAPIKeysUpdate), deps.Idempotency, m.handler.Update)
	group.Delete("/:id", authz.Require(authz.PermissionAdminAPIKeysRevoke), deps.Idempotency, m.handler.Revoke)
	group.Post("/:id/revoke", authz.Require(authz.PermissionAdminAPIKeysRevoke), deps.Idempotency, m.handler.RevokeByPost)
	group.Post("/:id/rotate", authz.Require(authz.PermissionAdminAPIKeysRotate), m.handler.Rotate)
}
