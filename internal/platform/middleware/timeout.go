package middleware

import (
	"time"

	"github.com/gofiber/fiber/v3"
	fibertimeout "github.com/gofiber/fiber/v3/middleware/timeout"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

// Timeout enforces a server-side deadline for the whole middleware chain and
// handler. The handler runs in the timeout middleware's managed goroutine so
// that a handler which ignores c.Context() cannot hold the response open past
// the deadline. On expiry, errorHandler renders the platform's RFC 9457
// system.timeout problem (504) before Fiber abandons the in-flight context.
func Timeout(duration time.Duration, errorHandler fiber.ErrorHandler) fiber.Handler {
	if errorHandler == nil {
		errorHandler = apperrorhttp.FiberErrorHandler
	}

	return fibertimeout.New(
		func(c fiber.Ctx) error { return c.Next() },
		fibertimeout.Config{
			Timeout: duration,
			OnTimeout: func(c fiber.Ctx) error {
				return errorHandler(c, apperror.NewCode(apperror.CodeSystemTimeout, "request timed out"))
			},
		},
	)
}
