// Package tenant holds the framework-free tenant identity: context helpers,
// the Resolver contract, and slug validation. Identity and file repositories
// consume the context ID so every query is scoped to one tenant. The Fiber
// middleware that resolves the request tenant lives in tenant/tenanthttp.
package tenant

import (
	"context"
	"regexp"
	"strings"
)

// Header selects the tenant for public auth/registration requests.
const Header = "X-Tenant-Slug"

// QueryParam is the validated browser-navigation fallback for flows where a
// browser cannot send Header (Google OAuth start, reset/verification links).
const QueryParam = "tenant"

// DefaultSlug identifies the tenant that preserves pre-multi-tenant behavior.
const DefaultSlug = "default"

type contextKey struct{}
type contextSlugKey struct{}

// DefaultID is the tenant used by pre-multi-tenant rows, background jobs, and
// tests that do not install tenant middleware. HTTP requests always resolve an
// explicit tenant; production requires Header so a missing tenant can never
// silently write to this compatibility tenant.
const DefaultID int64 = 1

// WithID stores the tenant ID in a context so services and repositories can
// scope queries without threading a parameter through every signature. A zero
// ID means "unset": EffectiveID applies the compatibility fallback for row
// parameters, while the row-level-security resolver
// (app.RLSResolver/tenant.IDFromContext) fails closed and sees no tenant rows.
// Negative IDs are preserved so validation boundaries can reject them instead
// of silently writing to the default tenant; tenant resolution never produces
// one.
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

// IsValidSlug reports whether slug is a valid external tenant identifier.
func IsValidSlug(slug string) bool {
	return slugPattern.MatchString(slug)
}
