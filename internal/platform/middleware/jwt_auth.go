package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

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

// unionAMR returns the de-duplicated union of the token's amr and the session's
// current amr, preserving the token's order first.
func unionAMR(tokenAMR, sessionAMR []string) []string {
	if len(sessionAMR) == 0 {
		return tokenAMR
	}
	out := make([]string, 0, len(tokenAMR)+len(sessionAMR))
	seen := make(map[string]struct{}, len(tokenAMR)+len(sessionAMR))
	for _, list := range [][]string{tokenAMR, sessionAMR} {
		for _, method := range list {
			if _, ok := seen[method]; ok {
				continue
			}
			seen[method] = struct{}{}
			out = append(out, method)
		}
	}
	return out
}

// JWTAuth validates the bearer token against a live session: the session must
// be unrevoked, the membership and user active, the tenant operational, and
// the token version current. The store also supplies the effective permission
// snapshot; there is no session-less mode, because a token alone would skip
// every one of those checks.
//
// The global tenant middleware runs first and resolves X-Tenant-Slug (or the
// configured default) into the tenant_id local. A token's positive tenant_id
// claim is authoritative for authenticated requests; an explicitly supplied
// header that resolves to a different tenant is rejected with 403. Tokens
// minted before multi-tenancy carry no tenant claim and are rejected
// fail-closed: deployments must rotate those tokens after upgrading.
func JWTAuth(secret string, store session.Store) fiber.Handler {
	return JWTAuthKeys(token.NewKeyring(secret), store)
}

// JWTAuthKeys is JWTAuth verifying against a keyring, so tokens signed with
// a previous JWT secret (JWT_PREVIOUS_SECRETS) stay valid during a rotation.
func JWTAuthKeys(keys *token.Keyring, store session.Store) fiber.Handler {
	if keys == nil {
		panic("middleware: JWTAuthKeys requires a keyring")
	}
	if store == nil {
		panic("middleware: JWTAuthKeys requires a session store")
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

		if claims.TenantID == uuid.Nil {
			return apperror.NewUnauthorized("token is missing tenant")
		}

		// Only an explicit header can conflict. The tenant middleware also
		// resolves an absent header to the default tenant, which must not make
		// every non-default JWT fail with 403. tenant_id is authoritative for
		// scoping; tenant_slug must describe that same tenant. When the claim
		// overrides a header-less default resolution we do not know the slug,
		// so clear it instead of leaving a misleading "default".
		headerSlug := strings.TrimSpace(c.Get(tenant.Header))
		resolvedTenantID := tenant.IDFromContext(c.Context())
		if headerSlug != "" && resolvedTenantID != claims.TenantID {
			return apperror.NewCode(apperror.CodePermissionDenied, "tenant mismatch")
		}
		if headerSlug == "" && resolvedTenantID != uuid.Nil && resolvedTenantID != claims.TenantID {
			c.SetContext(tenant.WithSlug(c.Context(), ""))
		}
		c.SetContext(tenant.WithID(c.Context(), claims.TenantID))

		// The role comes from the session snapshot only; a legacy role claim is
		// not parsed into the token claims.
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
		subject := authz.Subject{
			UserID:        claims.UserID,
			TenantID:      claims.TenantID,
			Role:          state.Role,
			Permissions:   permissions,
			CrossTenant:   state.Platform,
			MFAVerifiedAt: state.MFAVerifiedAt,
		}
		// A step-up updates the live session's amr. The access token is a
		// snapshot minted earlier, so the current session's methods count too (a
		// refreshed token repeats them anyway).
		effectiveAMR := unionAMR(claims.AMR, state.AMR)

		c.SetContext(requestctx.WithSessionID(c.Context(), claims.SessionID))
		c.SetContext(apikey.WithAuthMethod(c.Context(), apikey.AuthMethodJWT))
		authzhttp.SetSubject(c, subject.WithAMR(effectiveAMR))

		// Services publish events with the request context, so the actor must
		// be available there too.
		c.SetContext(requestctx.WithActorID(c.Context(), claims.UserID))

		// Tier 1 services write their audit rows inside the mutation
		// transaction; they need the actor and request metadata in the
		// context passed to the service.
		ctx := audit.WithActor(c.Context(), claims.UserID, subject.Role)
		ctx = audit.WithRequestMeta(ctx,
			requestctx.RequestID(c.Context()), c.IP(), c.Get("User-Agent"))
		c.SetContext(ctx)

		return c.Next()
	}
}
