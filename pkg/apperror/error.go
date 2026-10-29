// Package apperror defines transport-agnostic domain errors. An AppError
// carries an HTTP-style status as data, a stable code, public detail, and
// optional i18n keys; pkg/apperror/apperrorhttp renders it as an RFC 9457
// problem document for Fiber, and internal/platform/httpapi renders it for
// huma operations.
package apperror

import (
	"errors"
	"net/http"
	"time"
)

// ErrNotFound is a sentinel error returned by repositories when a record is not found.
// Services should check errors.Is(err, ErrNotFound) instead of importing database drivers.
var ErrNotFound = errors.New("record not found")

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
// Code is a stable dotted string usable as an i18n key on the client.
//
// DetailKey optionally names a catalog message used to localize Detail. When
// no Localizer is configured, or the key is absent, the literal Detail is
// preserved, so existing callers keep their current behavior.
//
// Cause is the internal error chain. It is logged for 5xx responses and is
// never rendered: Detail is the only public text.
type AppError struct {
	Status       int
	Code         string
	Title        string
	Detail       string
	DetailKey    string
	DetailParams map[string]any
	Errors       []FieldError
	Cause        error
}

// WithDetailKey marks Detail as a catalog message. The key and params are used
// only when a Localizer is installed; otherwise the literal Detail remains.
func (e *AppError) WithDetailKey(key string, params map[string]any) *AppError {
	e.DetailKey = key
	e.DetailParams = params
	return e
}

func (e *AppError) Error() string { return e.Detail }

// Unwrap exposes the internal cause to errors.Is/As. The cause is never
// included in the response.
func (e *AppError) Unwrap() error { return e.Cause }

// Is makes a 404 AppError match ErrNotFound, so errors.Is(err, ErrNotFound)
// means "not found" whether err is the repository sentinel or a service's
// apperror.NewNotFound. No other status matches any sentinel.
func (e *AppError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// GetStatus makes AppError a huma.StatusError, so a handler returning it keeps
// its status when huma writes the error. A status-less error built by NewCode
// reports 500 until a Catalog renders it.
func (e *AppError) GetStatus() int {
	if e.Status == 0 {
		return http.StatusInternalServerError
	}
	return e.Status
}

// Problem renders an error that already carries its presentation (the
// NewForStatus family). Catalog-owned codes must render through
// Catalog.Problem so the injected catalog decides status, title, and type.
func (e *AppError) Problem(requestID, instance string) ProblemDetails {
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	title := e.Title
	if title == "" {
		title = "Internal Server Error"
	}
	return ProblemDetails{
		Type:      "/errors/" + slugCode(e.Code),
		Title:     title,
		Status:    status,
		Code:      e.Code,
		Detail:    e.Detail,
		Instance:  instance,
		RequestID: requestID,
		Timestamp: timestamp(),
		Errors:    normalizeFieldPaths(e.Errors),
	}
}

func timestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func newError(status int, code, title, detail string) *AppError {
	return &AppError{Status: status, Code: code, Title: title, Detail: detail}
}

// NewBadRequest returns 400 request.invalid.
func NewBadRequest(detail string) *AppError {
	return newError(http.StatusBadRequest, string(CodeRequestInvalid), "Bad Request", detail)
}

// NewUnauthorized returns 401 auth.unauthenticated.
func NewUnauthorized(detail string) *AppError {
	return newError(http.StatusUnauthorized, string(CodeAuthUnauthenticated), "Unauthorized", detail)
}

// NewForbidden returns 403 permission.denied. Use a catalog code when the
// refusal has a more specific meaning.
func NewForbidden(detail string) *AppError {
	return newError(http.StatusForbidden, string(CodePermissionDenied), "Forbidden", detail)
}

// NewNotFound returns 404 resource.not_found.
func NewNotFound(detail string) *AppError {
	return newError(http.StatusNotFound, string(CodeResourceNotFound), "Not Found", detail)
}

// NewConflict returns 409 resource.conflict.
func NewConflict(detail string) *AppError {
	return newError(http.StatusConflict, string(CodeResourceConflict), "Conflict", detail)
}

// NewInternal returns 500 system.internal without a cause. Prefer Internal
// when wrapping an underlying error so the cause is logged.
func NewInternal(detail string) *AppError {
	return newError(http.StatusInternalServerError, string(CodeSystemInternal), "Internal Server Error", detail)
}

// Internal returns 500 system.internal and keeps err as the log-only cause.
// detail is public text: never include the cause, SQL, or stack traces in it.
func Internal(err error, detail string) *AppError {
	e := NewInternal(detail)
	e.Cause = err
	return e
}

// NewPayloadTooLarge returns 413 request.payload_too_large.
func NewPayloadTooLarge(detail string) *AppError {
	return newError(http.StatusRequestEntityTooLarge, string(CodeRequestPayloadTooLarge), "Payload Too Large", detail)
}

// NewUnsupportedMediaType returns 415 request.unsupported_media_type.
func NewUnsupportedMediaType(detail string) *AppError {
	return newError(http.StatusUnsupportedMediaType, string(CodeRequestUnsupportedMediaType), "Unsupported Media Type", detail)
}

// NewTooManyRequests returns 429 rate_limit.exceeded.
func NewTooManyRequests(detail string) *AppError {
	return newError(http.StatusTooManyRequests, string(CodeRateLimitExceeded), "Too Many Requests", detail)
}

// NewPreconditionFailed returns 412 resource.precondition_failed (for example a
// stale If-Match).
func NewPreconditionFailed(detail string) *AppError {
	return newError(http.StatusPreconditionFailed, string(CodeResourcePreconditionFailed), "Precondition Failed", detail)
}

// NewValidation builds a 422 request.validation_failed with a flat list of
// field errors.
func NewValidation(detail string, fields []FieldError) *AppError {
	e := newError(http.StatusUnprocessableEntity, string(CodeRequestValidationFailed), "Validation failed", detail)
	e.Errors = fields
	return e
}

// NewForStatus maps a bare HTTP status (for example a Fiber or huma error) to
// the one catalog code for that meaning. Unknown 4xx statuses become
// request.invalid and unknown 5xx statuses system.internal, so no response
// ever carries an ad-hoc snake_case code.
func NewForStatus(status int, detail string) *AppError {
	switch status {
	case http.StatusBadRequest:
		return NewBadRequest(detail)
	case http.StatusUnauthorized:
		return NewUnauthorized(detail)
	case http.StatusForbidden:
		return NewForbidden(detail)
	case http.StatusNotFound:
		return NewNotFound(detail)
	case http.StatusMethodNotAllowed:
		return newError(status, string(CodeRequestMethodNotAllowed), "Method Not Allowed", detail)
	case http.StatusNotAcceptable:
		return newError(status, string(CodeRequestNotAcceptable), "Not Acceptable", detail)
	case http.StatusConflict:
		return NewConflict(detail)
	case http.StatusGone:
		return newError(status, string(CodeResourceGone), "Gone", detail)
	case http.StatusPreconditionFailed:
		return NewPreconditionFailed(detail)
	case http.StatusRequestEntityTooLarge:
		return NewPayloadTooLarge(detail)
	case http.StatusUnsupportedMediaType:
		return NewUnsupportedMediaType(detail)
	case http.StatusUnprocessableEntity:
		return NewValidation(detail, nil)
	case http.StatusTooManyRequests:
		return NewTooManyRequests(detail)
	case http.StatusGatewayTimeout:
		return NewCode(CodeSystemTimeout, detail)
	case StatusClientClosedRequest:
		return newError(status, string(CodeRequestCanceled), "Client Closed Request", detail)
	case http.StatusServiceUnavailable:
		return NewCode(CodeSystemUnavailable, detail)
	case http.StatusBadGateway:
		return newError(status, string(CodeSystemBadGateway), "Bad Gateway", detail)
	}
	if status >= 500 {
		return NewInternal(detail)
	}
	if status >= 400 {
		return NewBadRequest(detail)
	}
	return NewInternal(detail)
}

// slugCode converts a dotted code to the legacy DocPath slug for a code with no
// catalog spec. Registered codes always use their Spec.DocPath.
func slugCode(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		switch code[i] {
		case '.':
			out = append(out, '-')
		case '_':
			out = append(out, '-')
		default:
			out = append(out, code[i])
		}
	}
	return string(out)
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
	Timestamp string       `json:"timestamp" format:"date-time" doc:"RFC 3339 timestamp."`
	Errors    []FieldError `json:"errors,omitempty" nullable:"false" doc:"Field-level errors when present; omitted otherwise, never null."`
}

// Error makes ProblemDetails an error value.
func (p *ProblemDetails) Error() string { return p.Detail }

// GetStatus makes ProblemDetails a huma.StatusError.
func (p *ProblemDetails) GetStatus() int { return p.Status }

// ContentType ensures huma writes RFC 9457 responses for its own validation
// errors as well as handler errors.
func (p *ProblemDetails) ContentType(ct string) string {
	if ct == "application/json" {
		return "application/problem+json"
	}
	return ct
}
