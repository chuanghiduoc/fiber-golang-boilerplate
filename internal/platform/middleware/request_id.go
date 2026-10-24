package middleware

import (
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
)

// validRequestID accepts a bounded, log-safe correlation ID from an upstream
// proxy. Arbitrary client headers are never echoed into logs/responses.
func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		requestID := c.Get("X-Request-Id")
		if !validRequestID(requestID) {
			requestID = uuid.New().String()
		}

		c.Set("X-Request-Id", requestID)
		fiber.Locals[string](c, "request_id", requestID)

		// Also set in context.Context for service/repository layer access.
		c.SetContext(requestctx.WithRequestID(c.Context(), requestID))

		return c.Next()
	}
}
