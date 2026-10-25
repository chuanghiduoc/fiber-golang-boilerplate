// Package apikeyhttp is the Fiber adapter for internal/platform/apikey: the
// Authenticate middleware that accepts an API key or delegates to JWT auth.
package apikeyhttp

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Authenticate accepts either an API key or a JWT. Requests using
// `Authorization: ApiKey <key>` are verified by verifier; every other
// Authorization value (including Bearer and a missing header) is delegated to
// jwtHandler, so route behavior for existing clients is unchanged.
//
// On API-key success the middleware sets user_id, email, role, tenant_id,
// api_key_id, auth_method, and an authz.Subject whose explicit permissions are
// the key's scopes intersected with the owner's current role. A key can
// therefore never exceed its recorded scopes or the owner's current
// permissions, even when its owner is an administrator.
func Authenticate(jwtHandler fiber.Handler, verifier *apikey.Verifier) fiber.Handler {
	return func(c fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		scheme, credential, ok := parseAuthorization(header)
		if !ok || !strings.EqualFold(scheme, apikey.APIKeyScheme) {
			// Missing headers, Bearer tokens, and unknown schemes keep their
			// existing JWT error behavior.
			if jwtHandler == nil {
				return apperror.NewUnauthorized("invalid authorization header format")
			}
			return jwtHandler(c)
		}
		if verifier == nil {
			return apperror.NewUnauthorized("invalid API key")
		}
		if credential == "" {
			return apperror.NewUnauthorized("invalid authorization header format")
		}

		// Only an explicit header can conflict. The tenant middleware resolves
		// an absent header to the default tenant; that must not make a
		// non-default key fail. The key's tenant is authoritative otherwise.
		headerSlug := strings.TrimSpace(c.Get(tenant.Header))
		expectedTenantID := int64(0)
		if headerSlug != "" {
			expectedTenantID = tenanthttp.ID(c)
		}

		verified, err := verifier.Verify(c.Context(), credential, expectedTenantID)
		if err != nil {
			return err
		}

		key := verified.Key
		principal := verified.Principal

		// Mirror JWTAuth: when the key's tenant overrides a header-less
		// default resolution, the resolved slug describes another tenant, so
		// clear it rather than leave a misleading value for link builders.
		resolvedTenantID := tenanthttp.ID(c)
		if headerSlug == "" && resolvedTenantID != 0 && resolvedTenantID != key.TenantID {
			fiber.Locals[string](c, "tenant_slug", "")
			c.SetContext(tenant.WithSlug(c.Context(), ""))
		}

		fiber.Locals[int64](c, apikey.LocalAPIKeyID, key.ID)
		fiber.Locals[string](c, apikey.LocalAuthMethod, apikey.AuthMethodAPIKey)
		fiber.Locals[int64](c, "user_id", principal.ID)
		fiber.Locals[string](c, "email", principal.Email)
		fiber.Locals[string](c, "role", principal.Role)
		fiber.Locals[int64](c, "tenant_id", key.TenantID)
		c.SetContext(tenant.WithID(c.Context(), key.TenantID))

		subject := authz.NewSubjectWithPermissions(principal.ID, key.TenantID, principal.Role, effectiveScopes(principal.Role, key.Scopes)...).
			WithAttribute(authz.AttributeAuthMethod, apikey.AuthMethodAPIKey).
			WithAttribute(authz.AttributeAPIKeyID, strconv.FormatInt(key.ID, 10))
		authzhttp.SetSubject(c, subject)

		// Services publish events and Tier 1 audit rows from the request
		// context, so the actor and request metadata must be there too, exactly
		// as JWTAuth installs them. The audit actor model has no credential
		// field; the key id stays on the authz subject (AttributeAPIKeyID) and
		// the api_key_id local.
		c.SetContext(requestctx.WithActorID(c.Context(), principal.ID))
		ctx := audit.WithActor(c.Context(), principal.ID, principal.Role)
		ctx = audit.WithRequestMeta(ctx,
			fiber.Locals[string](c, "request_id"), c.IP(), c.Get("User-Agent"))
		c.SetContext(ctx)

		return c.Next()
	}
}

// effectiveScopes intersects the key's stored scopes with the owner's current
// role. A key issued while its owner was an administrator therefore loses
// admin capabilities immediately after a demotion, matching the principal
// revalidation performed on every request.
func effectiveScopes(role string, scopes []authz.Permission) []authz.Permission {
	if role == "" {
		return scopes
	}
	effective := make([]authz.Permission, 0, len(scopes))
	for _, scope := range scopes {
		if authz.Has(role, scope) {
			effective = append(effective, scope)
		}
	}
	return effective
}

func parseAuthorization(header string) (scheme, credential string, ok bool) {
	if header == "" {
		return "", "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return parts[0], "", true
	}
	return parts[0], strings.TrimSpace(parts[1]), true
}
