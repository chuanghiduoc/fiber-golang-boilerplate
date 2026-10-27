package middleware

import (
	"strings"

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

// RequestID assigns the request correlation ID. An inbound X-Request-Id is
// honored only from a trusted proxy (APP_TRUST_PROXY with the peer in
// APP_TRUSTED_PROXIES); any other client gets a generated ID, so it cannot
// forge or collide correlation IDs in logs, audit rows, and traces.
func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		var requestID string
		if c.IsProxyTrusted() {
			// Clone: Fiber header strings are zero-copy views into a pooled
			// buffer (Immutable=false), but the ID outlives the request in
			// contexts, async goroutines, and exported span attributes.
			requestID = strings.Clone(c.Get("X-Request-Id"))
		}
		if !validRequestID(requestID) {
			requestID = uuid.New().String()
		}

		c.Set("X-Request-Id", requestID)

		// Also set in context.Context for service/repository layer access.
		ctx := requestctx.WithRequestID(c.Context(), requestID)
		ctx = requestctx.WithPath(ctx, c.Path())
		c.SetContext(ctx)

		return c.Next()
	}
}
