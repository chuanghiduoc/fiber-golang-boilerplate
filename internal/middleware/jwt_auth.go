package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func JWTAuth(secret string) fiber.Handler {
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

		fiber.Locals[int64](c, "user_id", claims.UserID)
		fiber.Locals[string](c, "email", claims.Email)
		fiber.Locals[string](c, "role", claims.Role)

		return c.Next()
	}
}
