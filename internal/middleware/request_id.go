package middleware

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

type contextKey string

const RequestIDKey contextKey = "request_id"

func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		requestID := c.Get("X-Request-Id")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		c.Set("X-Request-Id", requestID)
		fiber.Locals[string](c, "request_id", requestID)

		// Also set in context.Context for service/repository layer access
		ctx := context.WithValue(c.Context(), RequestIDKey, requestID)
		c.SetContext(ctx)

		return c.Next()
	}
}
