package authz

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// roleLocalKey is the Fiber locals key populated by the JWTAuth middleware.
const roleLocalKey = "role"

// Require returns middleware that allows the request only when the role stored
// in Fiber locals by JWTAuth grants permission. It must run after JWTAuth.
// A missing role or a denied permission yields a 403 problem response.
func Require(permission Permission) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !Has(fiber.Locals[string](c, roleLocalKey), permission) {
			return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
		}
		return c.Next()
	}
}

// RequireAny returns middleware that allows the request when the authenticated
// role grants at least one of the supplied permissions. An empty list denies
// every request. It must run after JWTAuth.
func RequireAny(permissions ...Permission) fiber.Handler {
	return func(c fiber.Ctx) error {
		role := fiber.Locals[string](c, roleLocalKey)
		for _, permission := range permissions {
			if Has(role, permission) {
				return c.Next()
			}
		}
		return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
	}
}
