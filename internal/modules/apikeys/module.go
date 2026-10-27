package apikeys

import (
	"io/fs"
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	apikeysprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module owns tenant-scoped API-key administration. Keys authenticate through
// the platform api-key middleware on other modules; this module only manages
// their lifecycle.
type Module struct {
	handler      *apikeyhandler.Handler
	privacy      *apikeysprivacy.Provider
	tenantPurger tenancycontracts.Purger
	provides     Provides
}

var (
	_ app.RouteProvider   = (*Module)(nil)
	_ app.PrivacyProvider = (*Module)(nil)
)

func New(handler *apikeyhandler.Handler) *Module {
	return &Module{handler: handler}
}

// Verifier returns the API-key request verifier.
func (m *Module) Verifier() *apikey.Verifier { return m.provides.Verifier }

func (m *Module) Name() string { return apikeyscatalog.Owner }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) Errors() []apperror.Spec { return apikeyscatalog.Errors() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return apikeyscatalog.Locales() }

// RegisterPrivacy implements app.PrivacyProvider.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "apikeys", Exporter: m.privacy, Eraser: m.privacy})
}

// RegisterRoutes implements app.RouteProvider: the caller's own keys live under
// /me/api-keys, another member's keys under /members/{memberId}/api-keys. The
// service enforces ownership and rank below the route policy, so an api_keys:*
// permission never reaches a key the caller does not own.
func (m *Module) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	tags := []string{"api-keys"}

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodGet,
		Path:        "/api/v1/me/api-keys",
		OperationID: "listMyAPIKeys",
		Tags:        tags,
		Summary:     "List the caller's API keys",
		Description: "Lists only keys owned by the authenticated caller, newest first. Secrets are never returned.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAPIKeysRead,
		RateLimit:  httpapi.RateLimitRelaxed,
	}, m.handler.ListMine)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/me/api-keys",
		OperationID: "createAPIKey",
		Tags:        tags,
		Summary:     "Create an API key",
		Description: "Creates a key owned by the authenticated caller. The plaintext key is returned once and never stored. Scopes must be held by the caller.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAPIKeysCreate,
		RateLimit:  httpapi.RateLimitNormal,
		NoStore:    true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "api_key.created"},
	}, m.handler.CreateMine)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodGet,
		Path:        "/api/v1/me/api-keys/{keyId}",
		OperationID: "getMyAPIKey",
		Tags:        tags,
		Summary:     "Get the caller's API key",
		Description: "Returns metadata for one key owned by the authenticated caller. Another member's key id is 404.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAPIKeysRead,
		RateLimit:  httpapi.RateLimitRelaxed,
	}, m.handler.GetMine)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPatch,
		Path:        "/api/v1/me/api-keys/{keyId}",
		OperationID: "updateMyAPIKey",
		Tags:        tags,
		Summary:     "Update the caller's API key",
		Description: "RFC 7396 merge patch of name and/or scopes. Scopes must remain within the owner's grants.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAPIKeysUpdate,
		RateLimit:  httpapi.RateLimitNormal,
		Idempotent: true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "api_key.updated"},
	}, m.handler.UpdateMine)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodDelete,
		Path:        "/api/v1/me/api-keys/{keyId}",
		OperationID: "deleteMyAPIKey",
		Tags:        tags,
		Summary:     "Revoke the caller's API key",
		Description: "Revokes a key owned by the authenticated caller immediately. Revocation is idempotent.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAPIKeysRevoke,
		RateLimit:  httpapi.RateLimitNormal,
		Idempotent: true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "api_key.revoked"},
	}, m.handler.DeleteMine)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/me/api-keys/{keyId}/rotate",
		OperationID: "rotateMyAPIKey",
		Tags:        tags,
		Summary:     "Rotate the caller's API key",
		Description: "Issues a replacement key for a key owned by the authenticated caller. The previous key keeps a grace window unless revoked. The new plaintext key is returned once.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAPIKeysRotate,
		RateLimit:  httpapi.RateLimitNormal,
		NoStore:    true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "api_key.rotated"},
	}, m.handler.RotateMine)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodGet,
		Path:        "/api/v1/members/{memberId}/api-keys",
		OperationID: "listMemberAPIKeys",
		Tags:        tags,
		Summary:     "List a member's API keys",
		Description: "Lists the keys owned by one member of the request tenant. Secrets are never returned.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAdminAPIKeysRead,
		RateLimit:  httpapi.RateLimitRelaxed,
	}, m.handler.ListMember)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPatch,
		Path:        "/api/v1/members/{memberId}/api-keys/{keyId}",
		OperationID: "updateMemberAPIKey",
		Tags:        tags,
		Summary:     "Update a member's API key",
		Description: "RFC 7396 merge patch of name and/or scopes. The caller must outrank the key's owner, and may narrow but never widen scopes.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAdminAPIKeysUpdate,
		RateLimit:  httpapi.RateLimitNormal,
		Idempotent: true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "api_key.updated"},
	}, m.handler.UpdateMember)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodDelete,
		Path:        "/api/v1/members/{memberId}/api-keys/{keyId}",
		OperationID: "deleteMemberAPIKey",
		Tags:        tags,
		Summary:     "Revoke a member's API key",
		Description: "Revokes a key owned by one member. The caller must outrank the key's owner. Revocation is idempotent.",
	}, httpapi.Policy{
		Permission: apikeyscatalog.PermissionAdminAPIKeysRevoke,
		RateLimit:  httpapi.RateLimitNormal,
		Idempotent: true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "api_key.revoked"},
	}, m.handler.DeleteMember)
}
