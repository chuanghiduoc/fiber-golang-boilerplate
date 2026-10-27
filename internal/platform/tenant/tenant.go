// Package tenant holds the framework-free tenant identity: context helpers,
// the Resolver contract, and slug validation. Identity and file repositories
// consume the context ID so every query is scoped to one tenant. The Fiber
// middleware that resolves the request tenant lives in tenant/tenanthttp.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Header selects the tenant for public auth/registration requests.
const Header = "X-Tenant-Slug"

// QueryParam is the validated browser-navigation fallback for flows where a
// browser cannot send Header (Google OAuth start, reset/verification links).
const QueryParam = "tenant"

// DefaultSlug identifies the seeded default tenant.
const DefaultSlug = "default"

type contextKey struct{}
type contextSlugKey struct{}

// DefaultID is the id of the seeded default tenant (slug "default"). It is a
// real tenant row, not a compatibility fallback: a missing tenant is an error.
// The same UUIDv7 constant is hard-coded in the init migration's seed.
var DefaultID = uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001")

// ErrMissingTenant is returned by Require when a tenant-owned read or write
// runs without a tenant in its context. HTTP requests resolve one in
// tenanthttp.Middleware and background work must install the job's tenant
// explicitly; there is no compatibility fallback.
var ErrMissingTenant = errors.New("tenant: no tenant in context")

// WithID stores the tenant ID in a context so services and repositories can
// scope queries without threading a parameter through every signature. The
// zero UUID means "unset" and is an error for tenant-owned work (see Require);
// the row-level-security resolver also fails closed and sees no tenant rows.
func WithID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// WithSlug stores the resolved tenant slug in a context. It is used to build
// reset/verification links that carry their tenant through browser navigation.
// An empty slug clears any previous value.
func WithSlug(ctx context.Context, slug string) context.Context {
	return context.WithValue(ctx, contextSlugKey{}, strings.TrimSpace(slug))
}

// WithIdentity stores both the tenant ID and slug in one context.
func WithIdentity(ctx context.Context, id uuid.UUID, slug string) context.Context {
	return WithSlug(WithID(ctx, id), slug)
}

// IDFromContext returns the tenant ID stored by WithID, or uuid.Nil.
func IDFromContext(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(contextKey{}).(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// SlugFromContext returns the tenant slug stored by WithSlug, or "".
func SlugFromContext(ctx context.Context) string {
	slug, _ := ctx.Value(contextSlugKey{}).(string)
	return slug
}

// Require returns the context tenant or ErrMissingTenant. Repositories and
// services that read or write tenant-owned rows must use it instead of a
// fallback: a zero tenant would make the row invisible under RLS or violate
// the tenant_id foreign key, so failing loudly is the only correct behaviour.
func Require(ctx context.Context) (uuid.UUID, error) {
	id := IDFromContext(ctx)
	if id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w (tenant id %s)", ErrMissingTenant, id)
	}
	return id, nil
}

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Tenant is the minimal tenant identity needed by request handling.
type Tenant struct {
	ID   uuid.UUID
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
