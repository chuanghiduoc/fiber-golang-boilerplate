package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// JWTAuth validates the bearer token and, when a session store is supplied,
// confirms that the account still exists and that its role/token version match
// the claims. The optional variadic form keeps tests and simple deployments
// working without a session store.
//
// The global tenant middleware runs first and resolves X-Tenant-Slug (or the
// default tenant) into the tenant_id local. A token's tenant_id claim is
// authoritative for authenticated requests; an explicitly supplied header
// that resolves to a different tenant is rejected with 403. Tokens minted
// before multi-tenancy carry no tenant claim and are rejected fail-closed:
// deployments must rotate those tokens after upgrading.
func JWTAuth(secret string, stores ...session.Store) fiber.Handler {
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

		claims, err := token.Parse(parts[1], secret)
		if err != nil {
			return apperror.NewUnauthorized("invalid or expired token")
		}

		if claims.TenantID == 0 {
			return apperror.NewUnauthorized("token is missing tenant")
		}

		// Only an explicit header can conflict. The tenant middleware also
		// resolves an absent header to the default tenant, which must not make
		// every non-default JWT fail with 403. tenant_slug keeps the
		// request-resolution value; tenant_id is authoritative for scoping.
		headerSlug := strings.TrimSpace(c.Get(tenant.Header))
		headerTenantID := fiber.Locals[int64](c, "tenant_id")
		if headerSlug != "" && headerTenantID != 0 && headerTenantID != claims.TenantID {
			return apperror.NewCode(apperror.CodePermissionDenied, "tenant mismatch")
		}
		fiber.Locals[int64](c, "tenant_id", claims.TenantID)
		c.SetContext(tenant.WithID(c.Context(), claims.TenantID))

		if store != nil {
			if err := store.Validate(c.Context(), claims.UserID, claims.TenantID, claims.Role, claims.TokenVersion); err != nil {
				return err
			}
		}

		fiber.Locals[int64](c, "user_id", claims.UserID)
		fiber.Locals[string](c, "email", claims.Email)
		fiber.Locals[string](c, "role", claims.Role)

		// Tier 1 services write their audit rows inside the mutation
		// transaction; they need the actor and request metadata in the
		// context passed to the service.
		ctx := audit.WithActor(c.Context(), claims.UserID, claims.Role)
		ctx = audit.WithRequestMeta(ctx,
			fiber.Locals[string](c, "request_id"), c.IP(), c.Get("User-Agent"))
		c.SetContext(ctx)

		return c.Next()
	}
}
