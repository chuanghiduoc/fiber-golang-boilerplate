package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// LocalSessionID is the Fiber local holding the authenticated token's
// session id (0 for API keys).
const LocalSessionID = "session_id"

// JWTAuth validates the bearer token and, when a session store is supplied,
// confirms that its session is live, the membership and user are active, and
// that the token version still matches. A session store also supplies the
// permission snapshot. The optional variadic form keeps tests and simple
// deployments working without a session store.
//
// The global tenant middleware runs first and resolves X-Tenant-Slug (or the
// configured default) into the tenant_id local. A token's positive tenant_id
// claim is authoritative for authenticated requests; an explicitly supplied
// header that resolves to a different tenant is rejected with 403. Tokens
// minted before multi-tenancy carry no tenant claim and are rejected
// fail-closed: deployments must rotate those tokens after upgrading.
func JWTAuth(secret string, stores ...session.Store) fiber.Handler {
	return JWTAuthKeys(token.NewKeyring(secret), stores...)
}

// JWTAuthKeys is JWTAuth verifying against a keyring, so tokens signed with
// a previous JWT secret (JWT_PREVIOUS_SECRETS) stay valid during a rotation.
func JWTAuthKeys(keys *token.Keyring, stores ...session.Store) fiber.Handler {
	if keys == nil {
		panic("middleware: JWTAuthKeys requires a keyring")
	}
	var store session.Store
	if len(stores) > 0 {
		store = stores[0]
	}

	return func(c fiber.Ctx) error {
		authHeader := c.Get("Authorization")
		if authHeader == "" {
			return apperror.NewUnauthorized("missing authorization header")
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return apperror.NewUnauthorized("invalid authorization header format")
		}

		claims, err := keys.Parse(parts[1])
		if err != nil {
			return apperror.NewUnauthorized("invalid or expired token")
		}

		if claims.TenantID <= 0 {
			return apperror.NewUnauthorized("token is missing tenant")
		}

		// Only an explicit header can conflict. The tenant middleware also
		// resolves an absent header to the default tenant, which must not make
		// every non-default JWT fail with 403. tenant_id is authoritative for
		// scoping; tenant_slug must describe that same tenant. When the claim
		// overrides a header-less default resolution we do not know the slug,
		// so clear it instead of leaving a misleading "default".
		headerSlug := strings.TrimSpace(c.Get(tenant.Header))
		resolvedTenantID := fiber.Locals[int64](c, "tenant_id")
		if headerSlug != "" && resolvedTenantID != claims.TenantID {
			return apperror.NewCode(apperror.CodePermissionDenied, "tenant mismatch")
		}
		if headerSlug == "" && resolvedTenantID != 0 && resolvedTenantID != claims.TenantID {
			fiber.Locals[string](c, "tenant_slug", "")
			c.SetContext(tenant.WithSlug(c.Context(), ""))
		}
		fiber.Locals[int64](c, "tenant_id", claims.TenantID)
		c.SetContext(tenant.WithID(c.Context(), claims.TenantID))

		subject := authz.NewSubject(claims.UserID, claims.TenantID, claims.Role)
		if store != nil {
			state, err := store.Validate(c.Context(), session.Claims{
				UserID:       claims.UserID,
				TenantID:     claims.TenantID,
				SessionID:    claims.SessionID,
				TokenVersion: claims.TokenVersion,
			})
			if err != nil {
				return err
			}
			permissions := make(map[authz.Permission]struct{}, len(state.Permissions))
			for _, permission := range state.Permissions {
				permissions[authz.Permission(permission)] = struct{}{}
			}
			subject = authz.Subject{
				UserID:      claims.UserID,
				TenantID:    claims.TenantID,
				Role:        state.Role,
				Permissions: permissions,
				CrossTenant: state.Platform,
			}
		}

		fiber.Locals[int64](c, "user_id", claims.UserID)
		fiber.Locals[int64](c, LocalSessionID, claims.SessionID)
		fiber.Locals[string](c, "email", claims.Email)
		fiber.Locals[string](c, "role", subject.Role)
		fiber.Locals[string](c, apikey.LocalAuthMethod, apikey.AuthMethodJWT)
		authzhttp.SetSubject(c, subject.WithAMR(claims.AMR))

		// Services publish events with the request context, so the actor must
		// be available there too, not only in Fiber locals.
		c.SetContext(requestctx.WithActorID(c.Context(), claims.UserID))

		// Tier 1 services write their audit rows inside the mutation
		// transaction; they need the actor and request metadata in the
		// context passed to the service.
		ctx := audit.WithActor(c.Context(), claims.UserID, subject.Role)
		ctx = audit.WithRequestMeta(ctx,
			fiber.Locals[string](c, "request_id"), c.IP(), c.Get("User-Agent"))
		c.SetContext(ctx)

		return c.Next()
	}
}
