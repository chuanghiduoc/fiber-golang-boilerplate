// Package requestctxhttp is the Fiber adapter for internal/platform/requestctx:
// it records transport-level client metadata once per request so handlers and
// services never read the HTTP framework directly.
package requestctxhttp

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
)

// Middleware stores the client IP and User-Agent in the request context.
//
// The IP comes from Fiber's c.IP(), which honors APP_TRUST_PROXY and
// APP_TRUSTED_PROXIES, so an X-Forwarded-For value is accepted only from a
// trusted peer. The strings are cloned because Fiber strings can alias pooled
// request buffers while the context outlives the request.
func Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.SetContext(requestctx.WithClientInfo(c.Context(), requestctx.ClientInfo{
			IP:        strings.Clone(strings.TrimSpace(c.IP())),
			UserAgent: strings.Clone(strings.TrimSpace(c.Get(fiber.HeaderUserAgent))),
		}))
		return c.Next()
	}
}
