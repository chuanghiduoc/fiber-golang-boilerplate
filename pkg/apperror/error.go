package apperror

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// ErrNotFound is a sentinel error returned by repositories when a record is not found.
// Services should check errors.Is(err, ErrNotFound) instead of importing database drivers.
var ErrNotFound = errors.New("record not found")

type AppError struct {
	Code      int    `json:"-"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	Details   any    `json:"details,omitempty"`
}

func (e *AppError) Error() string {
	return e.Message
}

func NewBadRequest(msg string) *AppError {
	return &AppError{
		Code:      fiber.StatusBadRequest,
		ErrorCode: "BAD_REQUEST",
		Message:   msg,
	}
}

func NewUnauthorized(msg string) *AppError {
	return &AppError{
		Code:      fiber.StatusUnauthorized,
		ErrorCode: "UNAUTHORIZED",
		Message:   msg,
	}
}

func NewForbidden(msg string) *AppError {
	return &AppError{
		Code:      fiber.StatusForbidden,
		ErrorCode: "FORBIDDEN",
		Message:   msg,
	}
}

func NewNotFound(msg string) *AppError {
	return &AppError{
		Code:      fiber.StatusNotFound,
		ErrorCode: "NOT_FOUND",
		Message:   msg,
	}
}

func NewInternal(msg string) *AppError {
	return &AppError{
		Code:      fiber.StatusInternalServerError,
		ErrorCode: "INTERNAL_ERROR",
		Message:   msg,
	}
}

func NewValidation(msg string, details any) *AppError {
	return &AppError{
		Code:      fiber.StatusUnprocessableEntity,
		ErrorCode: "VALIDATION_ERROR",
		Message:   msg,
		Details:   details,
	}
}

func FiberErrorHandler(c fiber.Ctx, err error) error {
	var appErr *AppError
	if errors.As(err, &appErr) {
		if appErr.Details != nil {
			return response.ErrorWithDetails(c, appErr.Code, appErr.ErrorCode, appErr.Message, appErr.Details)
		}
		return response.Error(c, appErr.Code, appErr.ErrorCode, appErr.Message)
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return response.Error(c, fiberErr.Code, "FIBER_ERROR", fiberErr.Message)
	}

	slog.Error("unhandled error in error handler",
		slog.String("error", err.Error()),
		slog.String("type", fmt.Sprintf("%T", err)),
		slog.String("path", c.Path()),
	)
	return response.Error(c, fiber.StatusInternalServerError, "INTERNAL_ERROR", "Internal Server Error")
}
