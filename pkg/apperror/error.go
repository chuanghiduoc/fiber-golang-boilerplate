package apperror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

// ErrNotFound is a sentinel error returned by repositories when a record is not found.
// Services should check errors.Is(err, ErrNotFound) instead of importing database drivers.
var ErrNotFound = errors.New("record not found")

// DocsBaseURL is prepended to the Problem Details "type" URI (RFC 9457).
// Set once at startup from ERROR_DOCS_BASE_URL; empty yields relative "/errors/...".
var DocsBaseURL = ""

// problemContentType is the RFC 9457 media type for error responses.
const problemContentType = "application/problem+json"

// StatusClientClosedRequest is the de-facto (nginx) status for a request whose
// client disconnected before the response. It is a client outcome, so it is
// logged at Info and never counted as a server error.
const StatusClientClosedRequest = 499

// FieldError is one entry in the flat errors[] list (validation / business rule).
// Params carries interpolation values (for example min/max) for localizers; it
// is intentionally excluded from the wire contract.
type FieldError struct {
	Path    string            `json:"path"`
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Params  map[string]string `json:"-"`
}

// AppError is a domain error that renders as an RFC 9457 problem document.
// Code is a stable snake_case string usable as an i18n key on the client.
//
// DetailKey optionally names a catalog message used to localize Detail. When
// no Localizer is configured, or the key is absent, the literal Detail is
// preserved, so existing callers keep their current behavior.
type AppError struct {
	Status       int
	Code         string
	Title        string
	Detail       string
	DetailKey    string
	DetailParams map[string]any
	Errors       []FieldError
}

// Localizer translates a message key for one request. Localize returns "" when
// the key is unknown, allowing the error handler to keep its English fallback.
// The i18n bundle implements this interface; tests may provide a fake.
type Localizer interface {
	Localize(c fiber.Ctx, key string, params map[string]any) string
}

// WithDetailKey marks Detail as a catalog message. The key and params are used
// only when a Localizer is installed; otherwise the literal Detail remains.
func (e *AppError) WithDetailKey(key string, params map[string]any) *AppError {
	e.DetailKey = key
	e.DetailParams = params
	return e
}

func (e *AppError) Error() string { return e.Detail }

// Is makes a 404 AppError match ErrNotFound, so errors.Is(err, ErrNotFound)
// means "not found" whether err is the repository sentinel or a service's
// apperror.NewNotFound. No other status matches any sentinel.
func (e *AppError) Is(target error) bool {
	return target == ErrNotFound && e.Status == fiber.StatusNotFound
}

func newError(status int, code, title, detail string) *AppError {
	return &AppError{Status: status, Code: code, Title: title, Detail: detail}
}

func NewBadRequest(detail string) *AppError {
	return newError(fiber.StatusBadRequest, "bad_request", "Bad Request", detail)
}

func NewUnauthorized(detail string) *AppError {
	return newError(fiber.StatusUnauthorized, "unauthorized", "Unauthorized", detail)
}

func NewForbidden(detail string) *AppError {
	return newError(fiber.StatusForbidden, "forbidden", "Forbidden", detail)
}

func NewNotFound(detail string) *AppError {
	return newError(fiber.StatusNotFound, "not_found", "Not Found", detail)
}

func NewConflict(detail string) *AppError {
	return newError(fiber.StatusConflict, "conflict", "Conflict", detail)
}

func NewInternal(detail string) *AppError {
	return newError(fiber.StatusInternalServerError, "internal_error", "Internal Server Error", detail)
}

func NewPayloadTooLarge(detail string) *AppError {
	return newError(fiber.StatusRequestEntityTooLarge, "payload_too_large", "Payload Too Large", detail)
}

func NewUnsupportedMediaType(detail string) *AppError {
	return newError(fiber.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported Media Type", detail)
}

func NewTooManyRequests(detail string) *AppError {
	return newError(fiber.StatusTooManyRequests, "too_many_requests", "Too Many Requests", detail)
}

func NewEmailNotVerified(detail string) *AppError {
	return newError(fiber.StatusForbidden, "email_not_verified", "Email not verified", detail)
}

// NewValidation builds a 422 with a flat list of field errors.
func NewValidation(detail string, fields []FieldError) *AppError {
	e := newError(fiber.StatusUnprocessableEntity, "validation_failed", "Validation failed", detail)
	e.Errors = fields
	return e
}

// ProblemDetails is the on-the-wire RFC 9457 error shape (application/problem+json).
type ProblemDetails struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Code      string       `json:"code"`
	Detail    string       `json:"detail"`
	Instance  string       `json:"instance"`
	RequestID string       `json:"requestId"`
	Timestamp string       `json:"timestamp"`
	Errors    []FieldError `json:"errors,omitempty"`
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
	var appErr *AppError
	if errors.As(err, &appErr) {
		return writeProblem(c, appErr, localizer)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return writeProblem(c, newError(fiber.StatusGatewayTimeout, "gateway_timeout", "Gateway Timeout", "request timed out"), localizer)
	}

	if errors.Is(err, context.Canceled) {
		// The client went away; the body is most likely never delivered.
		slog.Info("request canceled by client", slog.String("path", c.Path()))
		return writeProblem(c, newError(StatusClientClosedRequest, "client_closed_request", "Client Closed Request", "request canceled by client"), localizer)
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
	return writeProblem(c, NewInternal("Internal Server Error"), localizer)
}

func writeProblem(c fiber.Ctx, e *AppError, localizer Localizer) error {
	docPath := "/errors/" + strings.ReplaceAll(e.Code, "_", "-")
	if spec, ok := SpecFor(Code(e.Code)); ok {
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
			fields = make([]FieldError, len(e.Errors))
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

	doc := ProblemDetails{
		Type:      DocsBaseURL + docPath,
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

// fromStatus derives an AppError from a bare HTTP status (e.g. a fiber.Error).
func fromStatus(status int, detail string) *AppError {
	// Fiber rejects bodies over APP_BODY_LIMIT before any middleware runs.
	// Render that 413 with the same code as the BodyLimit middleware so
	// clients see one payload_too_large contract, not request_entity_too_large.
	if status == fiber.StatusRequestEntityTooLarge {
		if detail == "" || detail == http.StatusText(status) {
			detail = "request body too large"
		}
		return NewPayloadTooLarge(detail)
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
