package middleware

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// RequireRole returns a middleware that checks if the authenticated user has one of the allowed roles.
// Must be used after JWTAuth middleware.
func RequireRole(roles ...string) fiber.Handler {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}

	return func(c fiber.Ctx) error {
		role := fiber.Locals[string](c, "role")
		if _, ok := allowed[role]; !ok {
			return apperror.NewForbidden("insufficient permissions")
		}
		return c.Next()
	}
}
