package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// JWTAuth validates the bearer token and, when a session store is supplied,
// confirms that the account still exists and that its role/token version match
// the claims. The optional variadic form keeps tests and simple deployments
// working without a session store.
func JWTAuth(secret string, stores ...session.Store) fiber.Handler {
	var store session.Store
	if len(stores) > 0 {
		store = stores[0]
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

		claims, err := token.Parse(parts[1], secret)
		if err != nil {
			return apperror.NewUnauthorized("invalid or expired token")
		}

		if store != nil {
			if err := store.Validate(c.Context(), claims.UserID, claims.Role, claims.TokenVersion); err != nil {
				return err
			}
		}

		fiber.Locals[int64](c, "user_id", claims.UserID)
		fiber.Locals[string](c, "email", claims.Email)
		fiber.Locals[string](c, "role", claims.Role)

		return c.Next()
	}
}
