package apikeys

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
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

func (m *Module) Name() string { return apikeyscatalog.Owner }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	// API-key callers may manage keys: the admin:api_keys:* scopes gate each
	// route, and create/update/rotate reject any key scope the caller (key)
	// does not itself hold, so a key cannot mint a broader key.
	group := v1.Group("/admin/api-keys", deps.Authenticate(), deps.Limiters.Normal)

	group.Get("/", authz.Require(apikeyscatalog.PermissionAdminAPIKeysRead), m.handler.List)
	group.Post("/", authz.Require(apikeyscatalog.PermissionAdminAPIKeysCreate), m.handler.Create)
	group.Get("/:id", authz.Require(apikeyscatalog.PermissionAdminAPIKeysRead), m.handler.Get)
	group.Put("/:id", authz.Require(apikeyscatalog.PermissionAdminAPIKeysUpdate), deps.Idempotency, m.handler.Update)
	group.Delete("/:id", authz.Require(apikeyscatalog.PermissionAdminAPIKeysRevoke), deps.Idempotency, m.handler.Revoke)
	group.Post("/:id/revoke", authz.Require(apikeyscatalog.PermissionAdminAPIKeysRevoke), deps.Idempotency, m.handler.RevokeByPost)
	group.Post("/:id/rotate", authz.Require(apikeyscatalog.PermissionAdminAPIKeysRotate), m.handler.Rotate)
}
