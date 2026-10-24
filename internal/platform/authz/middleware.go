package authz

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// roleLocalKey is the Fiber locals key populated by the JWTAuth middleware.
const roleLocalKey = "role"

// requestSubject returns the subject installed by authentication middleware.
// When only a role local is present (older tests or third-party middleware),
// it derives permissions from the role catalog so route behavior is unchanged.
func requestSubject(c fiber.Ctx) (Subject, bool) {
	if subject, ok := SubjectFromFiber(c); ok {
		return subject, true
	}
	role := fiber.Locals[string](c, roleLocalKey)
	if role == "" {
		return Subject{}, false
	}
	return NewSubject(0, 0, role), true
}

// Require returns middleware that allows the request only when the
// authenticated subject grants permission. It must run after authentication.
// API-key subjects use their explicit scopes; JWT subjects derive permissions
// from their role. A missing subject or a denied permission yields a 403
// problem response.
func Require(permission Permission) fiber.Handler {
	return func(c fiber.Ctx) error {
		subject, ok := requestSubject(c)
		if !ok || !subject.Has(permission) {
			return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
		}
		return c.Next()
	}
}

// RequireAny returns middleware that allows the request when the authenticated
// subject grants at least one of the supplied permissions. An empty list denies
// every request. It must run after authentication.
func RequireAny(permissions ...Permission) fiber.Handler {
	return func(c fiber.Ctx) error {
		subject, ok := requestSubject(c)
		if !ok {
			return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
		}
		for _, permission := range permissions {
			if subject.Has(permission) {
				return c.Next()
			}
		}
		return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
	}
}
