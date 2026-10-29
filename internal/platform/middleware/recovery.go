package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func Recovery(env string) fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				// Recovery is outermost: a panic before RequestID or
				// SecurityHeaders must still produce a correlated problem
				// document with the baseline headers. Both helpers are
				// idempotent, so normal requests are unaffected.
				ensureRequestID(c)
				applySecurityHeaders(c, env)
				// A handler can panic after writing a partial success body. The
				// error handler will set the status and problem document, but it
				// cannot replace bytes already queued, so discard them first.
				if c.Written() {
					c.ResetBody()
				}
				stackTrace := debug.Stack()

				slog.ErrorContext(c.Context(), "panic recovered",
					slog.String("request_id", requestctx.RequestID(c.Context())),
					slog.Any("error", r),
					slog.String("stack", string(stackTrace)),
				)

				msg := "internal server error"
				if env == "local" || env == "test" {
					// Show panic details only in local/test environments
					msg = "internal server error (check server logs for details)"
				}

				err = apperror.NewInternal(msg)
			}
		}()

		ensureRequestID(c)
		applySecurityHeaders(c, env)
		return c.Next()
	}
}
