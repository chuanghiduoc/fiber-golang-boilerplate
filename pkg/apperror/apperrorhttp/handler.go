// Package apperrorhttp is the Fiber transport for pkg/apperror: it renders any
// error as an RFC 9457 application/problem+json document and localizes it per
// request. Business code returns *apperror.AppError and never imports this
// package; only the HTTP layer (app wiring, handler tests) does.
package apperrorhttp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// problemContentType is the RFC 9457 media type for error responses.
const problemContentType = "application/problem+json"

// Localizer translates a message key for one request. Localize returns "" when
// the key is unknown, allowing the error handler to keep its English fallback.
// The i18n bundle implements this interface; tests may provide a fake.
type Localizer interface {
	Localize(c fiber.Ctx, key string, params map[string]any) string
}

// FiberErrorHandler renders any error as an application/problem+json document.
// It is the nil-localizer path: titles/details and validation messages are
// emitted exactly as callers supplied them.
func FiberErrorHandler(c fiber.Ctx, err error) error {
	return handleError(c, err, nil)
}

// FiberErrorHandlerWithLocalizer returns an error handler that localizes
// catalog titles and opted-in details. Code, type, status, instance, and the
// errors[] shape are unchanged; a nil localizer is equivalent to
// FiberErrorHandler.
func FiberErrorHandlerWithLocalizer(localizer Localizer) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		return handleError(c, err, localizer)
	}
}

func handleError(c fiber.Ctx, err error, localizer Localizer) error {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return writeProblem(c, appErr, localizer)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return writeProblem(c, newError(fiber.StatusGatewayTimeout, "gateway_timeout", "Gateway Timeout", "request timed out"), localizer)
	}

	if errors.Is(err, context.Canceled) {
		// The client went away; the body is most likely never delivered.
		slog.Info("request canceled by client", slog.String("path", c.Path()))
		return writeProblem(c, newError(apperror.StatusClientClosedRequest, "client_closed_request", "Client Closed Request", "request canceled by client"), localizer)
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return writeProblem(c, fromStatus(fiberErr.Code, fiberErr.Message), localizer)
	}

	slog.Error("unhandled error in error handler",
		slog.String("error", err.Error()),
		slog.String("type", fmt.Sprintf("%T", err)),
		slog.String("path", c.Path()),
	)
	return writeProblem(c, apperror.NewInternal("Internal Server Error"), localizer)
}

func writeProblem(c fiber.Ctx, e *apperror.AppError, localizer Localizer) error {
	docPath := "/errors/" + strings.ReplaceAll(e.Code, "_", "-")
	if spec, ok := apperror.SpecFor(apperror.Code(e.Code)); ok {
		docPath = spec.DocPath
	}

	title := e.Title
	detail := e.Detail
	fields := e.Errors
	if localizer != nil {
		if localized := localizer.Localize(c, "errors."+e.Code+".title", nil); localized != "" {
			title = localized
		}
		if e.DetailKey != "" {
			if localized := localizer.Localize(c, e.DetailKey, e.DetailParams); localized != "" {
				detail = localized
			}
		}
		if len(e.Errors) > 0 {
			fields = make([]apperror.FieldError, len(e.Errors))
			for i, field := range e.Errors {
				params := make(map[string]any, len(field.Params)+1)
				params["path"] = field.Path
				for key, value := range field.Params {
					params[key] = value
				}
				if localized := localizer.Localize(c, "validation."+field.Code, params); localized != "" {
					field.Message = localized
				}
				fields[i] = field
			}
		}
	}

	doc := apperror.ProblemDetails{
		Type:      apperror.DocsBaseURL + docPath,
		Title:     title,
		Status:    e.Status,
		Code:      e.Code,
		Detail:    detail,
		Instance:  c.Path(),
		RequestID: requestID(c),
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Errors:    fields,
	}
	return c.Status(e.Status).JSON(doc, problemContentType)
}

func newError(status int, code, title, detail string) *apperror.AppError {
	return &apperror.AppError{Status: status, Code: code, Title: title, Detail: detail}
}

// fromStatus derives an AppError from a bare HTTP status (e.g. a fiber.Error).
func fromStatus(status int, detail string) *apperror.AppError {
	// Fiber rejects bodies over APP_BODY_LIMIT before any middleware runs.
	// Render that 413 with the same code as the BodyLimit middleware so
	// clients see one payload_too_large contract, not request_entity_too_large.
	if status == fiber.StatusRequestEntityTooLarge {
		if detail == "" || detail == http.StatusText(status) {
			detail = "request body too large"
		}
		return apperror.NewPayloadTooLarge(detail)
	}
	title := http.StatusText(status)
	if title == "" {
		title = "Error"
	}
	code := strings.ReplaceAll(strings.ToLower(title), " ", "_")
	if detail == "" {
		detail = title
	}
	return newError(status, code, title, detail)
}

// requestID mirrors the X-Request-Id set by the correlation middleware.
func requestID(c fiber.Ctx) string {
	if v := fiber.Locals[string](c, "request_id"); v != "" {
		return v
	}
	return c.Get("X-Request-Id")
}
