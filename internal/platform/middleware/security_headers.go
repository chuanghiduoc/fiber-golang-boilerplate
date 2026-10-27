package middleware

import (
	"github.com/gofiber/fiber/v3"
)

// SecurityHeaders applies the API security header baseline (OWASP HTTP
// Headers Cheat Sheet). It is a JSON API, so the CSP denies everything and
// only the framing and referrer hints add defense in depth.
func SecurityHeaders(appEnv string) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("X-Frame-Options", "DENY")
		// X-XSS-Protection is obsolete and its legacy auditor can introduce
		// vulnerabilities; explicitly disable it in browsers that still know it.
		c.Set("X-XSS-Protection", "0")
		c.Set("Referrer-Policy", "no-referrer")
		c.Set("Cross-Origin-Resource-Policy", "same-origin")
		c.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")

		if appEnv != "local" && appEnv != "test" {
			c.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		return c.Next()
	}
}
