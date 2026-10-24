// Package tenant resolves the request tenant and exposes it through Fiber
// locals and the request context. Identity and file repositories consume the
// resolved ID so every query is scoped to one tenant.
package tenant

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Header selects the tenant for public auth/registration requests.
const Header = "X-Tenant-Slug"

// QueryParam is the validated browser-navigation fallback for flows where a
// browser cannot send Header (Google OAuth start, reset/verification links).
const QueryParam = "tenant"

// DefaultSlug identifies the tenant that preserves pre-multi-tenant behavior.
const DefaultSlug = "default"

const (
	localTenantID   = "tenant_id"
	localTenantSlug = "tenant_slug"
)

type contextKey struct{}
type contextSlugKey struct{}

// DefaultID is the tenant used by pre-multi-tenant rows, background jobs, and
// tests that do not install tenant middleware. HTTP requests always resolve an
// explicit tenant; production requires Header so a missing tenant can never
// silently write to this compatibility tenant.
const DefaultID int64 = 1

// WithID stores the tenant ID in a context so services and repositories can
// scope queries without threading a parameter through every signature. A zero
// ID means "unset" and falls back to DefaultID. Negative IDs are preserved so
// validation boundaries can reject them instead of silently writing to the
// default tenant; tenant resolution never produces one.
func WithID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// WithSlug stores the resolved tenant slug in a context. It is used to build
// reset/verification links that carry their tenant through browser navigation.
// An empty slug clears any previous value.
func WithSlug(ctx context.Context, slug string) context.Context {
	return context.WithValue(ctx, contextSlugKey{}, strings.TrimSpace(slug))
}

// WithIdentity stores both the tenant ID and slug in one context.
func WithIdentity(ctx context.Context, id int64, slug string) context.Context {
	return WithSlug(WithID(ctx, id), slug)
}

// IDFromContext returns the tenant ID stored by WithID, or 0.
func IDFromContext(ctx context.Context) int64 {
	if id, ok := ctx.Value(contextKey{}).(int64); ok {
		return id
	}
	return 0
}

// SlugFromContext returns the tenant slug stored by WithSlug, or "".
func SlugFromContext(ctx context.Context) string {
	slug, _ := ctx.Value(contextSlugKey{}).(string)
	return slug
}

// EffectiveID returns the context tenant, falling back to DefaultID only when
// no tenant was stored. Background jobs, internal callers, and tests rely on
// that compatibility fallback. HTTP request paths must not rely on it: tenant
// middleware resolves the tenant and production requires Header
// (TENANT_REQUIRE_HEADER). A negative stored ID is returned unchanged so
// validation boundaries reject it rather than silently using tenant 1.
func EffectiveID(ctx context.Context) int64 {
	if id := IDFromContext(ctx); id != 0 {
		return id
	}
	return DefaultID
}

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Tenant is the minimal tenant identity needed by request handling.
type Tenant struct {
	ID   int64
	Slug string
	Name string
}

// Resolver looks up an active tenant by slug. Implementations must return
// apperror.ErrNotFound (or a wrapped error) when the slug is unknown or the
// tenant is suspended. Startup checks use a separate existence lookup.
type Resolver interface {
	Resolve(ctx context.Context, slug string) (Tenant, error)
}

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
func Middleware(r Resolver, defaultSlug string) fiber.Handler {
	if r == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}

	return func(c fiber.Ctx) error {
		path := canonicalPath(c.Path())
		if path == oauthCallbackPath || strings.HasPrefix(path, webhookInboundPrefix) {
			return c.Next()
		}

		slug := strings.TrimSpace(c.Get(Header))
		if slug == "" && allowsTenantQuery(path) {
			slug = strings.TrimSpace(c.Query(QueryParam))
		}
		if slug == "" {
			slug = defaultSlug
		}
		if !IsValidSlug(slug) {
			return apperror.NewNotFound("tenant not found")
		}

		resolved, err := r.Resolve(c.Context(), slug)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("tenant not found")
			}
			return apperror.NewInternal("failed to resolve tenant")
		}
		if resolved.ID <= 0 {
			return apperror.NewInternal("failed to resolve tenant")
		}

		fiber.Locals[int64](c, localTenantID, resolved.ID)
		fiber.Locals[string](c, localTenantSlug, resolved.Slug)
		c.SetContext(WithIdentity(c.Context(), resolved.ID, resolved.Slug))
		return c.Next()
	}
}

// ID returns the tenant ID resolved for the request, or 0 when no tenant
// middleware ran.
func ID(c fiber.Ctx) int64 {
	return fiber.Locals[int64](c, localTenantID)
}

// Slug returns the tenant slug resolved for the request, or "" when no tenant
// middleware ran (or when a JWT claim overrode an unresolved default).
func Slug(c fiber.Ctx) string {
	return fiber.Locals[string](c, localTenantSlug)
}

// IsValidSlug reports whether slug is a valid external tenant identifier.
func IsValidSlug(slug string) bool {
	return slugPattern.MatchString(slug)
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
	"/api/v1/auth/google":         {},
	"/api/v1/auth/reset-password": {},
	"/api/v1/auth/verify-email":   {},
}

func allowsTenantQuery(path string) bool {
	_, ok := tenantQueryPaths[path]
	return ok
}

func canonicalPath(path string) string {
	if len(path) > 1 {
		return strings.TrimRight(path, "/")
	}
	return path
}
