package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func Recovery(env string) fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				stackTrace := debug.Stack()

				// request_id is empty when the panic happened before the
				// RequestID middleware ran (outermost Recovery).
				slog.Error("panic recovered",
					slog.String("request_id", fiber.Locals[string](c, "request_id")),
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

		return c.Next()
	}
}
