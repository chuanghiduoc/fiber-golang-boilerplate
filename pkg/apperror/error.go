// Package apperror defines transport-agnostic domain errors. An AppError
// carries an HTTP-style status as data, a stable code, public detail, and
// optional i18n keys; pkg/apperror/apperrorhttp renders it as an RFC 9457
// problem document for Fiber.
package apperror

import (
	"errors"
	"net/http"
)

// ErrNotFound is a sentinel error returned by repositories when a record is not found.
// Services should check errors.Is(err, ErrNotFound) instead of importing database drivers.
var ErrNotFound = errors.New("record not found")

// DocsBaseURL is prepended to the Problem Details "type" URI (RFC 9457).
// Set once at startup from ERROR_DOCS_BASE_URL; empty yields relative "/errors/...".
var DocsBaseURL = ""

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
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

func newError(status int, code, title, detail string) *AppError {
	return &AppError{Status: status, Code: code, Title: title, Detail: detail}
}

func NewBadRequest(detail string) *AppError {
	return newError(http.StatusBadRequest, "bad_request", "Bad Request", detail)
}

func NewUnauthorized(detail string) *AppError {
	return newError(http.StatusUnauthorized, "unauthorized", "Unauthorized", detail)
}

func NewForbidden(detail string) *AppError {
	return newError(http.StatusForbidden, "forbidden", "Forbidden", detail)
}

func NewNotFound(detail string) *AppError {
	return newError(http.StatusNotFound, "not_found", "Not Found", detail)
}

func NewConflict(detail string) *AppError {
	return newError(http.StatusConflict, "conflict", "Conflict", detail)
}

func NewInternal(detail string) *AppError {
	return newError(http.StatusInternalServerError, "internal_error", "Internal Server Error", detail)
}

func NewPayloadTooLarge(detail string) *AppError {
	return newError(http.StatusRequestEntityTooLarge, "payload_too_large", "Payload Too Large", detail)
}

func NewUnsupportedMediaType(detail string) *AppError {
	return newError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Unsupported Media Type", detail)
}

func NewTooManyRequests(detail string) *AppError {
	return newError(http.StatusTooManyRequests, "too_many_requests", "Too Many Requests", detail)
}

func NewEmailNotVerified(detail string) *AppError {
	return newError(http.StatusForbidden, "email_not_verified", "Email not verified", detail)
}

// NewValidation builds a 422 with a flat list of field errors.
func NewValidation(detail string, fields []FieldError) *AppError {
	e := newError(http.StatusUnprocessableEntity, "validation_failed", "Validation failed", detail)
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
