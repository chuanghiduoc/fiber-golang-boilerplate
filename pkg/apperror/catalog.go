package apperror

import (
	"net/http"
	"sort"
)

// Code is a stable, dotted identifier for a domain error condition.
// Codes are part of the public API contract: clients may branch on them,
// so they must never change once released.
type Code string

// Stable domain error codes.
const (
	CodeAuthInvalidCredentials Code = "auth.invalid_credentials"
	CodeAuthEmailNotVerified   Code = "auth.email_not_verified"
	CodeAuthRefreshInvalid     Code = "auth.refresh_invalid"

	CodeUserEmailTaken Code = "user.email_taken"
	CodeUserNotFound   Code = "user.not_found"
	CodeUserLastAdmin  Code = "user.last_admin"

	CodeFileNotFound        Code = "file.not_found"
	CodeFileTooLarge        Code = "file.too_large"
	CodeFileUnsupportedType Code = "file.unsupported_type"
	CodeFileNotDeleted      Code = "file.not_deleted"

	CodePermissionDenied  Code = "permission.denied"
	CodeRateLimitExceeded Code = "rate_limit.exceeded"

	CodeSystemInternal    Code = "system.internal"
	CodeSystemTimeout     Code = "system.timeout"
	CodeSystemUnavailable Code = "system.unavailable"
)

// Spec describes the stable, non-changing presentation of a Code.
// Detail text is never part of the Spec: it is caller-supplied and public.
type Spec struct {
	Status    int
	Title     string
	Retryable bool
	DocPath   string
}

// catalog is the single source of truth for every published Code.
// It is populated once at init and only read afterwards, so it is safe
// for concurrent use without locking.
var catalog = map[Code]Spec{
	CodeAuthInvalidCredentials: {
		Status:  http.StatusUnauthorized,
		Title:   "Invalid credentials",
		DocPath: "/errors/auth-invalid-credentials",
	},
	CodeAuthEmailNotVerified: {
		Status:  http.StatusForbidden,
		Title:   "Email not verified",
		DocPath: "/errors/auth-email-not-verified",
	},
	CodeAuthRefreshInvalid: {
		Status:  http.StatusUnauthorized,
		Title:   "Invalid refresh token",
		DocPath: "/errors/auth-refresh-invalid",
	},

	CodeUserEmailTaken: {
		Status:  http.StatusConflict,
		Title:   "Email already in use",
		DocPath: "/errors/user-email-taken",
	},
	CodeUserNotFound: {
		Status:  http.StatusNotFound,
		Title:   "User not found",
		DocPath: "/errors/user-not-found",
	},
	CodeUserLastAdmin: {
		Status:  http.StatusConflict,
		Title:   "Cannot remove the last administrator",
		DocPath: "/errors/user-last-admin",
	},

	CodeFileNotFound: {
		Status:  http.StatusNotFound,
		Title:   "File not found",
		DocPath: "/errors/file-not-found",
	},
	CodeFileTooLarge: {
		Status:  http.StatusRequestEntityTooLarge,
		Title:   "File too large",
		DocPath: "/errors/file-too-large",
	},
	CodeFileUnsupportedType: {
		Status:  http.StatusUnsupportedMediaType,
		Title:   "Unsupported file type",
		DocPath: "/errors/file-unsupported-type",
	},
	CodeFileNotDeleted: {
		Status:  http.StatusConflict,
		Title:   "File was not deleted",
		DocPath: "/errors/file-not-deleted",
	},

	CodePermissionDenied: {
		Status:  http.StatusForbidden,
		Title:   "Permission denied",
		DocPath: "/errors/permission-denied",
	},
	CodeRateLimitExceeded: {
		Status:    http.StatusTooManyRequests,
		Title:     "Rate limit exceeded",
		Retryable: true,
		DocPath:   "/errors/rate-limit-exceeded",
	},

	CodeSystemInternal: {
		Status:  http.StatusInternalServerError,
		Title:   "Internal server error",
		DocPath: "/errors/system-internal",
	},
	CodeSystemTimeout: {
		Status:    http.StatusGatewayTimeout,
		Title:     "Request timed out",
		Retryable: true,
		DocPath:   "/errors/system-timeout",
	},
	CodeSystemUnavailable: {
		Status:    http.StatusServiceUnavailable,
		Title:     "Service unavailable",
		Retryable: true,
		DocPath:   "/errors/system-unavailable",
	},
}

// SpecFor returns the Spec registered for code and whether it exists.
func SpecFor(code Code) (Spec, bool) {
	spec, ok := catalog[code]
	return spec, ok
}

// NewCode builds an *AppError from the catalog entry for code.
// detail is public, caller-supplied text for the end user; never pass
// internal causes, SQL, stack traces, or other sensitive data.
// Unknown codes fall back to 500 system.internal so a typo can never
// leak an undefined status or title to clients.
func NewCode(code Code, detail string) *AppError {
	spec, ok := SpecFor(code)
	if !ok {
		code = CodeSystemInternal
		spec = catalog[CodeSystemInternal]
	}
	return newError(spec.Status, string(code), spec.Title, detail)
}

// AllCodes returns every registered code in stable lexicographic order.
// The returned slice is a fresh copy, so callers may modify it freely.
func AllCodes() []Code {
	codes := make([]Code, 0, len(catalog))
	for code := range catalog {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}
