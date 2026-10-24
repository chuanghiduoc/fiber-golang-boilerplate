package middleware

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
)

// Timeout returns a middleware that sets a timeout on the request context.
func Timeout(duration time.Duration) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), duration)
		defer cancel()

		c.SetContext(ctx)
		return c.Next()
	}
}
