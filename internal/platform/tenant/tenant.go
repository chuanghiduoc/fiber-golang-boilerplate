// Package tenant resolves the request tenant and exposes it through Fiber
// locals. Module queries will consume this value; the foundation deliberately
// does not change existing query scoping yet.
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

// DefaultSlug identifies the tenant that preserves pre-multi-tenant behavior.
const DefaultSlug = "default"

const (
	localTenantID   = "tenant_id"
	localTenantSlug = "tenant_slug"
)

type contextKey struct{}

// DefaultID is the tenant used by pre-multi-tenant rows and tests that do not
// install tenant middleware. Production requests always carry an explicit ID.
const DefaultID int64 = 1

// WithID stores the tenant ID in a context so services and repositories can
// scope queries without threading a parameter through every signature.
func WithID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// IDFromContext returns the tenant ID stored by WithID, or 0.
func IDFromContext(ctx context.Context) int64 {
	if id, ok := ctx.Value(contextKey{}).(int64); ok {
		return id
	}
	return 0
}

// EffectiveID returns the context tenant, falling back to DefaultID for
// backward compatibility with tests and pre-tenant call sites.
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
// apperror.ErrNotFound (or a wrapped error) when the slug is unknown.
type Resolver interface {
	Resolve(ctx context.Context, slug string) (Tenant, error)
}

// Middleware resolves the tenant from X-Tenant-Slug and stores tenant_id and
// tenant_slug in Fiber locals. An empty header falls back to defaultSlug. A
// nil resolver is a pass-through so tests and minimal deployments do not need
// a database. Passing an empty defaultSlug requires the header (unknown or
// missing -> 404).
func Middleware(r Resolver, defaultSlug string) fiber.Handler {
	if r == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}

	return func(c fiber.Ctx) error {
		slug := strings.TrimSpace(c.Get(Header))
		if slug == "" {
			slug = defaultSlug
		}
		if !validSlug(slug) {
			return apperror.NewNotFound("tenant not found")
		}

		resolved, err := r.Resolve(c.Context(), slug)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("tenant not found")
			}
			return apperror.NewInternal("failed to resolve tenant")
		}

		fiber.Locals[int64](c, localTenantID, resolved.ID)
		fiber.Locals[string](c, localTenantSlug, resolved.Slug)
		c.SetContext(WithID(c.Context(), resolved.ID))
		return c.Next()
	}
}

// ID returns the tenant ID resolved for the request, or 0 when no tenant
// middleware ran.
func ID(c fiber.Ctx) int64 {
	return fiber.Locals[int64](c, localTenantID)
}

// Slug returns the tenant slug resolved for the request, or "" when no tenant
// middleware ran.
func Slug(c fiber.Ctx) string {
	return fiber.Locals[string](c, localTenantSlug)
}

func validSlug(slug string) bool {
	return slugPattern.MatchString(slug)
}
