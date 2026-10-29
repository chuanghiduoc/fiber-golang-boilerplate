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
		ensureRequestID(c)
		return c.Next()
	}
}

// ensureRequestID is idempotent so the outermost Recovery can install the ID
// before CORS and the remaining middleware run, and the RequestID middleware
// can reuse it instead of minting a second ID.
func ensureRequestID(c fiber.Ctx) string {
	if requestID := requestctx.RequestID(c.Context()); validRequestID(requestID) {
		c.Set("X-Request-Id", requestID)
		c.SetContext(requestctx.WithPath(c.Context(), c.Path()))
		return requestID
	}

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
	ctx := requestctx.WithRequestID(c.Context(), requestID)
	ctx = requestctx.WithPath(ctx, c.Path())
	c.SetContext(ctx)
	return requestID
}
