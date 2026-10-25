// Package featureshttp is the Fiber adapter for internal/platform/features:
// route middleware that gates a route on a flag. Services evaluate flags
// through features.Require on the request context and never import it.
package featureshttp

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
)

// Middleware gates a route on key for the request tenant and, when the route
// is authenticated, the JWT user. Mount it after tenant resolution and
// authentication (so user targets apply) and before idempotency/audit, so a
// disabled request neither caches a response nor writes an audit intent row.
func Middleware(flags features.Evaluator, key features.Key) fiber.Handler {
	return func(c fiber.Ctx) error {
		subject := features.SubjectFromContext(c.Context(), httpx.AuthUserID(c))
		if err := features.Require(c.Context(), flags, key, subject); err != nil {
			return err
		}
		return c.Next()
	}
}
