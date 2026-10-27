package middleware

import "github.com/gofiber/fiber/v3"

// NoStore marks a response as uncacheable. It is used for token- and
// secret-returning operations (login, refresh, API-key create/rotate, TOTP
// enrollment, webhook secrets, data-export downloads) and for any response that
// must not be retained by a shared cache, browser, or intermediary.
func NoStore() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")
		return c.Next()
	}
}
