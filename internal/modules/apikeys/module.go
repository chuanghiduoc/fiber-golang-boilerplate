package apikeys

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	apikeysprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
)

// Module owns tenant-scoped API-key administration. Keys authenticate through
// the platform api-key middleware on other modules; this module only manages
// their lifecycle.
type Module struct {
	handler      *apikeyhandler.Handler
	privacy      *apikeysprivacy.Provider
	tenantPurger tenancycontracts.Purger
}

var _ app.PrivacyProvider = (*Module)(nil)

func New(handler *apikeyhandler.Handler) *Module {
	return &Module{handler: handler}
}

func (m *Module) Name() string { return apikeyscatalog.Owner }

// RegisterPrivacy implements app.PrivacyProvider.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "apikeys", Exporter: m.privacy, Eraser: m.privacy})
}

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	// API-key callers may manage keys: the admin:api_keys:* scopes gate each
	// route, and create/update/rotate reject any key scope the caller (key)
	// does not itself hold, so a key cannot mint a broader key.
	group := v1.Group("/admin/api-keys", deps.Authenticate(), deps.Limiters.Normal)

	group.Get("/", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysRead), m.handler.List)
	group.Post("/", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysCreate), m.handler.Create)
	group.Get("/:id", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysRead), m.handler.Get)
	group.Put("/:id", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysUpdate), deps.Idempotency, m.handler.Update)
	group.Delete("/:id", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysRevoke), deps.Idempotency, m.handler.Revoke)
	group.Post("/:id/revoke", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysRevoke), deps.Idempotency, m.handler.RevokeByPost)
	group.Post("/:id/rotate", authzhttp.Require(apikeyscatalog.PermissionAdminAPIKeysRotate), m.handler.Rotate)
}
