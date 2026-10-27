// Package apperrorhttp is the Fiber transport for pkg/apperror: it renders any
// error as an RFC 9457 application/problem+json document and localizes it per
// request. Business code returns *apperror.AppError and never imports this
// package; only the HTTP layer (app wiring, handler tests) does.
package apperrorhttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
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

// New returns the injected-catalog error handler. The catalog is authoritative
// for status, title, and type; a nil catalog falls back to the platform-only
// catalog and a nil localizer keeps English titles/details.
func New(catalog *apperror.Catalog, localizer Localizer) fiber.ErrorHandler {
	if catalog == nil {
		catalog = apperror.PlatformCatalog()
	}
	return func(c fiber.Ctx, err error) error {
		return handleError(c, err, catalog, localizer)
	}
}

// FiberErrorHandler renders any error with the platform-only catalog. It is
// the nil-catalog/nil-localizer path for tests and edge callers; application
// wiring must use New so module codes resolve.
func FiberErrorHandler(c fiber.Ctx, err error) error {
	return handleError(c, err, apperror.PlatformCatalog(), nil)
}

// FiberErrorHandlerWithLocalizer is the platform-only catalog with
// localization. Application wiring uses New instead.
func FiberErrorHandlerWithLocalizer(localizer Localizer) fiber.ErrorHandler {
	return New(apperror.PlatformCatalog(), localizer)
}

func handleError(c fiber.Ctx, err error, catalog *apperror.Catalog, localizer Localizer) error {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return writeProblem(c, appErr, catalog, localizer)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return writeProblem(c, apperror.NewCode(apperror.CodeSystemTimeout, "request timed out"), catalog, localizer)
	}

	if errors.Is(err, context.Canceled) {
		// The client went away; the body is most likely never delivered.
		slog.Info("request canceled by client", slog.String("path", c.Path()))
		return writeProblem(c, apperror.NewForStatus(apperror.StatusClientClosedRequest, "request canceled by client"), catalog, localizer)
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		detail := fiberErr.Message
		// Fiber rejects bodies over APP_BODY_LIMIT before any middleware runs.
		// Render that 413 with the same code/detail as BodyLimit so clients see
		// one payload-too-large contract.
		if fiberErr.Code == fiber.StatusRequestEntityTooLarge &&
			(detail == "" || detail == http.StatusText(fiberErr.Code)) {
			detail = "request body too large"
		}
		return writeProblem(c, apperror.NewForStatus(fiberErr.Code, detail), catalog, localizer)
	}

	return writeProblem(c, apperror.Internal(err, "Internal Server Error"), catalog, localizer)
}

// logServerError records every 5xx with its internal cause. The cause is never
// copied into the problem document.
func logServerError(ctx context.Context, path string, err error) {
	slog.ErrorContext(ctx, "request failed with an internal error",
		slog.String("path", path),
		slog.String("request_id", requestctx.RequestID(ctx)),
		slog.Any("cause", err),
	)
}

func writeProblem(c fiber.Ctx, e *apperror.AppError, catalog *apperror.Catalog, localizer Localizer) error {
	doc := catalog.Problem(e, getRequestID(c), c.Path())
	if localizer != nil {
		if localized := localizer.Localize(c, "errors."+doc.Code+".title", nil); localized != "" {
			doc.Title = localized
		}
		if e.DetailKey != "" {
			if localized := localizer.Localize(c, e.DetailKey, e.DetailParams); localized != "" {
				doc.Detail = localized
			}
		}
		if len(doc.Errors) > 0 {
			fields := make([]apperror.FieldError, len(doc.Errors))
			for i, field := range doc.Errors {
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
			doc.Errors = fields
		}
	}

	if doc.Status >= http.StatusInternalServerError {
		cause := e.Cause
		if cause == nil {
			cause = e
		}
		logServerError(c.Context(), c.Path(), cause)
	}

	return c.Status(doc.Status).JSON(doc, problemContentType)
}

// getRequestID returns the correlation ID installed by the request-ID
// middleware. It deliberately does not fall back to the client's X-Request-Id
// header: an error rendered before that middleware ran must not echo an
// untrusted value into the problem document.
func getRequestID(c fiber.Ctx) string {
	return requestctx.RequestID(c.Context())
}
