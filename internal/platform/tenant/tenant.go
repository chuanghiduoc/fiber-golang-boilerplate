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
