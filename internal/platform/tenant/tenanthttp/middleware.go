// Package tenanthttp is the Fiber adapter for internal/platform/tenant: it
// resolves the request tenant and exposes it through Fiber locals and the
// request context. Services and repositories read the tenant from the context
// through package tenant and never import this package.
package tenanthttp

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Middleware resolves the tenant from X-Tenant-Slug and stores tenant_id and
// tenant_slug in Fiber locals and the request context. An empty header falls
// back to a validated `tenant` query parameter on browser-facing auth flows,
// then to defaultSlug. A nil resolver is a pass-through so tests and minimal
// deployments do not need a database. Passing an empty defaultSlug requires
// the header (unknown or missing -> 404).
//
// The Google OAuth callback is exempt: Google's redirect cannot carry a custom
// header, so the handler derives the tenant from the signed OAuth state and
// installs the context itself. Exempting it prevents require-header mode from
// rejecting the callback before that validation runs.
//
// The public webhook receiver is exempt for the same reason: its source row,
// authenticated by HMAC, selects the tenant, so requiring X-Tenant-Slug would
// make the endpoint unusable for third-party senders.
func Middleware(r tenant.Resolver, defaultSlug string) fiber.Handler {
	if r == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}

	return func(c fiber.Ctx) error {
		path := canonicalPath(c.Path())
		if path == oauthCallbackPath || strings.HasPrefix(path, webhookInboundPrefix) {
			return c.Next()
		}

		// c.Get and c.Query return zero-copy views into fasthttp's pooled
		// request buffer, which is reused by the next request on the
		// connection. The slug outlives this request (resolver cache keys,
		// locals), so it must be an owned copy.
		slug := strings.Clone(strings.TrimSpace(c.Get(tenant.Header)))
		if slug == "" && allowsTenantQuery(path) {
			slug = strings.Clone(strings.TrimSpace(c.Query(tenant.QueryParam)))
		}
		if slug == "" {
			slug = defaultSlug
		}
		if !tenant.IsValidSlug(slug) {
			return apperror.NewNotFound("tenant not found")
		}

		resolved, err := r.Resolve(c.Context(), slug)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("tenant not found")
			}
			return apperror.Internal(err, "failed to resolve tenant")
		}
		if resolved.ID == uuid.Nil {
			return apperror.NewInternal("failed to resolve tenant")
		}

		c.SetContext(tenant.WithIdentity(c.Context(), resolved.ID, resolved.Slug))
		return c.Next()
	}
}

// ID returns the tenant ID resolved for the request, or 0 when no tenant
// middleware ran.
func ID(c fiber.Ctx) uuid.UUID {
	return tenant.IDFromContext(c.Context())
}

// Slug returns the tenant slug resolved for the request, or "" when no tenant
// middleware ran (or when a JWT claim overrode an unresolved default).
func Slug(c fiber.Ctx) string {
	return tenant.SlugFromContext(c.Context())
}

const oauthCallbackPath = "/api/v1/auth/google/callback"

// webhookInboundPrefix is the public webhook receiver. The source row, not a
// request header, owns the tenant, so tenant middleware must not run first.
const webhookInboundPrefix = "/api/v1/webhooks/inbound/"

// tenantQueryPaths are public/browser-facing endpoints that may select their
// tenant with a validated query parameter because navigation or an emailed link
// cannot set X-Tenant-Slug. Authenticated routes never use this fallback; the
// JWT tenant_id claim remains authoritative there.
var tenantQueryPaths = map[string]struct{}{
	"/api/v1/auth/google":                       {},
	"/api/v1/auth/password-resets/complete":     {},
	"/api/v1/auth/email-verifications/complete": {},
}

func allowsTenantQuery(path string) bool {
	_, ok := tenantQueryPaths[path]
	return ok
}

// canonicalPath normalizes a request path for the exemption and tenant-query
// maps. Fiber routes case-insensitively and non-strictly, so the maps must
// compare the lowercase, slash-trimmed form; otherwise a mixed-case callback
// URL would miss its exemption and be rejected in require-header mode.
func canonicalPath(path string) string {
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return strings.ToLower(path)
}
