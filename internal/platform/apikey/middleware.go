package apikey

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// APIKeyScheme is the Authorization scheme for API-key credentials.
const APIKeyScheme = "ApiKey"

// Fiber locals populated on successful API-key authentication.
const (
	LocalAPIKeyID   = "api_key_id"
	LocalAuthMethod = "auth_method"
)

// Authentication methods recorded in locals/authz attributes.
const (
	AuthMethodAPIKey = "api_key"
	AuthMethodJWT    = "jwt"
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
func Authenticate(jwtHandler fiber.Handler, verifier *Verifier) fiber.Handler {
	return func(c fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		scheme, credential, ok := parseAuthorization(header)
		if !ok || !strings.EqualFold(scheme, APIKeyScheme) {
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
		expectedTenantID := int64(0)
		if strings.TrimSpace(c.Get(tenant.Header)) != "" {
			expectedTenantID = tenant.ID(c)
		}

		verified, err := verifier.Verify(c.Context(), credential, expectedTenantID)
		if err != nil {
			return err
		}

		key := verified.Key
		principal := verified.Principal

		fiber.Locals[int64](c, LocalAPIKeyID, key.ID)
		fiber.Locals[string](c, LocalAuthMethod, AuthMethodAPIKey)
		fiber.Locals[int64](c, "user_id", principal.ID)
		fiber.Locals[string](c, "email", principal.Email)
		fiber.Locals[string](c, "role", principal.Role)
		fiber.Locals[int64](c, "tenant_id", key.TenantID)
		c.SetContext(tenant.WithID(c.Context(), key.TenantID))

		subject := authz.NewSubjectWithPermissions(principal.ID, key.TenantID, principal.Role, effectiveScopes(principal.Role, key.Scopes)...).
			WithAttribute(authz.AttributeAuthMethod, AuthMethodAPIKey).
			WithAttribute(authz.AttributeAPIKeyID, strconv.FormatInt(key.ID, 10))
		authz.SetSubject(c, subject)

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
