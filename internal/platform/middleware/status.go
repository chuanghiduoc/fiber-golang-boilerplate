package middleware

import (
	"context"
	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// effectiveStatus returns the status the response will ultimately have. Fiber
// runs the ErrorHandler after middleware unwinds, so reading
// c.Response().StatusCode() inside Logger/Metrics would record handler errors
// as 200.
func effectiveStatus(catalog *apperror.Catalog, c fiber.Ctx, err error) int {
	if err == nil {
		return c.Response().StatusCode()
	}

	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		if catalog == nil {
			catalog = apperror.PlatformCatalog()
		}
		_, spec := catalog.Resolve(apperror.Code(appErr.Code))
		return spec.Status
	}
	// Mirror apperror's error handler precedence: an explicit AppError wins,
	// then context errors.
	if errors.Is(err, context.DeadlineExceeded) {
		return fiber.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return apperror.StatusClientClosedRequest
	}
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}
	return fiber.StatusInternalServerError
}
