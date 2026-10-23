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
func effectiveStatus(c fiber.Ctx, err error) int {
	if err == nil {
		return c.Response().StatusCode()
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fiber.StatusGatewayTimeout
	}

	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return appErr.Status
	}
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return fiberErr.Code
	}
	return fiber.StatusInternalServerError
}
