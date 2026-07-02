package apperror

import (
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

// FieldError is one entry in the flat errors[] list (validation / business rule).
type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AppError is a domain error that renders as an RFC 9457 problem document.
// Code is a stable snake_case string usable as an i18n key on the client.
type AppError struct {
	Status int
	Code   string
	Title  string
	Detail string
	Errors []FieldError
}

func (e *AppError) Error() string { return e.Detail }

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
func FiberErrorHandler(c fiber.Ctx, err error) error {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return writeProblem(c, appErr)
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return writeProblem(c, fromStatus(fiberErr.Code, fiberErr.Message))
	}

	slog.Error("unhandled error in error handler",
		slog.String("error", err.Error()),
		slog.String("type", fmt.Sprintf("%T", err)),
		slog.String("path", c.Path()),
	)
	return writeProblem(c, NewInternal("Internal Server Error"))
}

func writeProblem(c fiber.Ctx, e *AppError) error {
	doc := ProblemDetails{
		Type:      DocsBaseURL + "/errors/" + strings.ReplaceAll(e.Code, "_", "-"),
		Title:     e.Title,
		Status:    e.Status,
		Code:      e.Code,
		Detail:    e.Detail,
		Instance:  c.Path(),
		RequestID: requestID(c),
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Errors:    e.Errors,
	}
	return c.Status(e.Status).JSON(doc, problemContentType)
}

// fromStatus derives an AppError from a bare HTTP status (e.g. a fiber.Error).
func fromStatus(status int, detail string) *AppError {
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
