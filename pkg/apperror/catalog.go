package apperror

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Code is a stable, dotted identifier for a domain error condition.
// Codes are part of the public API contract: clients may branch on them,
// so they must never change once released.
type Code string

// Platform-wide error codes. Codes owned by a single business module are
// declared in that module's catalog package and registered at startup with
// Register (wired by app.RegisterCatalogs from app.ErrorProvider).
const (
	// CodeUserLastAdmin stays central because the identity repository (owned
	// by the schema migration work) still returns it directly; move it to the
	// identity catalog together with that repository change.
	CodeUserLastAdmin Code = "user.last_admin"

	CodePermissionDenied  Code = "permission.denied"
	CodeFeatureDisabled   Code = "feature.disabled"
	CodeRateLimitExceeded Code = "rate_limit.exceeded"

	CodeAPIVersionUnsupported Code = "api.version_unsupported"

	// CodeIdempotencyKeyMismatch is an Idempotency-Key reused with a different
	// request body (ADR 0008). A duplicate still in progress is a plain 409.
	CodeIdempotencyKeyMismatch Code = "idempotency.key_mismatch"

	CodeSystemInternal         Code = "system.internal"
	CodeSystemTimeout          Code = "system.timeout"
	CodeSystemUnavailable      Code = "system.unavailable"
	CodeSystemAuditUnavailable Code = "system.audit_unavailable"
)

// Spec describes the stable, non-changing presentation of a Code.
// Detail text is never part of the Spec: it is caller-supplied and public.
type Spec struct {
	Status    int
	Title     string
	Retryable bool
	DocPath   string
}

// platformOwner is the owner recorded for the platform-wide codes.
const platformOwner = "platform"

// codePattern keeps codes dotted and lower-case ("orders.not_found").
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

type catalogState struct {
	specs  map[Code]Spec
	owners map[Code]string
}

var (
	registerMu sync.Mutex
	// state is an immutable snapshot published by Register, so the error
	// rendering path reads it without locking.
	state atomic.Pointer[catalogState]
)

func init() {
	state.Store(&catalogState{specs: map[Code]Spec{}, owners: map[Code]string{}})
	if err := Register(platformOwner, platformSpecs); err != nil {
		panic(err)
	}
}

var platformSpecs = map[Code]Spec{
	CodeUserLastAdmin: {
		Status:  http.StatusConflict,
		Title:   "Cannot remove the last owner",
		DocPath: "/errors/user-last-admin",
	},

	CodePermissionDenied: {
		Status:  http.StatusForbidden,
		Title:   "Permission denied",
		DocPath: "/errors/permission-denied",
	},
	// A disabled feature behaves like an absent route (404), matching the
	// existing "Google OAuth not configured" response; the stable code still
	// lets clients tell it apart from a mistyped URL.
	CodeFeatureDisabled: {
		Status:  http.StatusNotFound,
		Title:   "Feature disabled",
		DocPath: "/errors/feature-disabled",
	},
	CodeRateLimitExceeded: {
		Status:    http.StatusTooManyRequests,
		Title:     "Rate limit exceeded",
		Retryable: true,
		DocPath:   "/errors/rate-limit-exceeded",
	},

	CodeAPIVersionUnsupported: {
		Status:  http.StatusNotAcceptable,
		Title:   "Unsupported API version",
		DocPath: "/errors/api-version-unsupported",
	},
	// 422 follows draft-ietf-httpapi-idempotency-key-header: the request is
	// well-formed but cannot be processed under a key bound to another body.
	CodeIdempotencyKeyMismatch: {
		Status:  http.StatusUnprocessableEntity,
		Title:   "Idempotency key reused with a different request",
		DocPath: "/errors/idempotency-key-mismatch",
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
	CodeSystemAuditUnavailable: {
		Status:    http.StatusServiceUnavailable,
		Title:     "Audit service unavailable",
		Retryable: true,
		DocPath:   "/errors/system-audit-unavailable",
	},
}

// Register adds owner's codes to the catalog. It fails on an invalid code or
// spec (status outside 400..599, empty title, DocPath without /errors/), and
// on a code already registered by another owner or with a different spec.
// Re-registering identical specs for the same owner is a no-op so the
// application can be built more than once per process (tests). A failed call
// registers nothing. Call it during startup, before serving requests.
func Register(owner string, specs map[Code]Spec) error {
	if owner == "" {
		return fmt.Errorf("apperror: register: owner is required")
	}
	registerMu.Lock()
	defer registerMu.Unlock()

	current := state.Load()
	next := &catalogState{
		specs:  make(map[Code]Spec, len(current.specs)+len(specs)),
		owners: make(map[Code]string, len(current.owners)+len(specs)),
	}
	for code, spec := range current.specs {
		next.specs[code] = spec
		next.owners[code] = current.owners[code]
	}
	for code, spec := range specs {
		if err := validateSpec(code, spec); err != nil {
			return fmt.Errorf("apperror: %s: %w", owner, err)
		}
		if existingOwner, exists := current.owners[code]; exists {
			if existingOwner != owner {
				return fmt.Errorf("apperror: code %q registered by %q is already owned by %q", code, owner, existingOwner)
			}
			if current.specs[code] != spec {
				return fmt.Errorf("apperror: %s: code %q re-registered with a different spec", owner, code)
			}
			continue
		}
		next.specs[code] = spec
		next.owners[code] = owner
	}
	state.Store(next)
	return nil
}

func validateSpec(code Code, spec Spec) error {
	if !codePattern.MatchString(string(code)) {
		return fmt.Errorf("invalid code %q (want dotted lower-case segments)", code)
	}
	if spec.Status < 400 || spec.Status > 599 {
		return fmt.Errorf("code %q: status %d outside 400..599", code, spec.Status)
	}
	if strings.TrimSpace(spec.Title) == "" {
		return fmt.Errorf("code %q: title is required", code)
	}
	if !strings.HasPrefix(spec.DocPath, "/errors/") {
		return fmt.Errorf("code %q: DocPath %q must start with /errors/", code, spec.DocPath)
	}
	return nil
}

// SpecFor returns the Spec registered for code and whether it exists.
func SpecFor(code Code) (Spec, bool) {
	spec, ok := state.Load().specs[code]
	return spec, ok
}

// OwnerOf returns the owner that registered code ("platform" for the
// platform-wide catalog) and whether code is known.
func OwnerOf(code Code) (string, bool) {
	owner, ok := state.Load().owners[code]
	return owner, ok
}

// NewCode builds an *AppError from the catalog entry for code.
// detail is public, caller-supplied text for the end user; never pass
// internal causes, SQL, stack traces, or other sensitive data.
// Unknown codes fall back to 500 system.internal so a typo can never
// leak an undefined status or title to clients; the fallback is logged at
// Error level because it means a module's ErrorProvider was not registered.
func NewCode(code Code, detail string) *AppError {
	spec, ok := SpecFor(code)
	if !ok {
		slog.Error("apperror: unregistered error code; responding with system.internal",
			slog.String("code", string(code)))
		code = CodeSystemInternal
		spec, _ = SpecFor(CodeSystemInternal)
	}
	return newError(spec.Status, string(code), spec.Title, detail)
}

// AllCodes returns every registered code in stable lexicographic order.
// The returned slice is a fresh copy, so callers may modify it freely.
func AllCodes() []Code {
	specs := state.Load().specs
	codes := make([]Code, 0, len(specs))
	for code := range specs {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}
