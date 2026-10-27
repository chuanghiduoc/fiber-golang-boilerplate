package apperror

import (
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Code is a stable, dotted identifier for a domain error condition.
// Codes are part of the public API contract: clients may branch on them,
// so they must never change once released.
type Code string

// Platform-wide error codes. Codes owned by a single business module are
// declared in that module's catalog package and contributed through
// app.ErrorProvider at the composition root.
const (
	// Generic request errors. Every status has exactly one code; do not add
	// aliases such as "bad_request" or "not_found".
	CodeRequestInvalid              Code = "request.invalid"
	CodeRequestValidationFailed     Code = "request.validation_failed"
	CodeRequestPayloadTooLarge      Code = "request.payload_too_large"
	CodeRequestUnsupportedMediaType Code = "request.unsupported_media_type"
	CodeRequestMethodNotAllowed     Code = "request.method_not_allowed"
	CodeRequestNotAcceptable        Code = "request.not_acceptable"
	CodeRequestCanceled             Code = "request.canceled"

	// Authentication and authorization.
	CodeAuthUnauthenticated Code = "auth.unauthenticated"
	CodePermissionDenied    Code = "permission.denied"
	// CodeMFARequired is enforced by the platform RequireMFA guard: the access
	// token lacks an otp/rc amr entry, or the session's mfa_verified_at is
	// missing or older than MFA_MAX_AGE. The stale case adds a detail pointing
	// at POST /me/mfa/step-up; the code is reused deliberately so clients
	// switch on one value.
	CodeMFARequired Code = "auth.mfa_required"

	// Resource errors.
	CodeResourceNotFound           Code = "resource.not_found"
	CodeResourceConflict           Code = "resource.conflict"
	CodeResourceGone               Code = "resource.gone"
	CodeResourcePreconditionFailed Code = "resource.precondition_failed"

	CodeFeatureDisabled   Code = "feature.disabled"
	CodeRateLimitExceeded Code = "rate_limit.exceeded"

	// CodeIdempotencyKeyMismatch is an Idempotency-Key reused with a different
	// request body. A duplicate still in progress is a plain 409.
	CodeIdempotencyKeyMismatch Code = "idempotency.key_mismatch"

	CodeSystemInternal         Code = "system.internal"
	CodeSystemTimeout          Code = "system.timeout"
	CodeSystemUnavailable      Code = "system.unavailable"
	CodeSystemBadGateway       Code = "system.bad_gateway"
	CodeSystemAuditUnavailable Code = "system.audit_unavailable"
)

// Spec describes the stable, non-changing presentation of a Code.
// Detail text is never part of the Spec: it is caller-supplied and public.
type Spec struct {
	Code      Code
	Status    int
	Title     string
	Retryable bool
	DocPath   string
}

// Provider contributes one owner's error specs to a Catalog. The composition
// root builds one provider per app.ErrorProvider plus the platform specs.
type Provider struct {
	Owner string
	Specs []Spec
}

// platformOwner is the owner recorded for the platform-wide codes.
const platformOwner = "platform"

// codePattern keeps codes dotted and lower-case ("orders.not_found").
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Catalog is an injected, instance-scoped error catalog. It is populated at
// the composition root (or by a test helper) and sealed before serving, so the
// per-request read path is lock-free after startup. There is deliberately no
// process-wide registry.
type Catalog struct {
	mu     sync.RWMutex
	specs  map[Code]Spec
	owners map[Code]string
	order  []Code
	sealed bool
}

// NewCatalog returns a catalog pre-populated with the platform-wide codes. It
// is unsealed so the composition root can add module providers.
func NewCatalog() *Catalog {
	specs := platformSpecs()
	c := &Catalog{
		specs:  make(map[Code]Spec, len(specs)),
		owners: make(map[Code]string, len(specs)),
	}
	// Platform specs are trusted constants; a failure here is a build bug.
	if err := c.Add(Provider{Owner: platformOwner, Specs: specs}); err != nil {
		panic(err)
	}
	return c
}

// PlatformCatalog returns a sealed catalog containing only platform-wide
// codes. It is the fallback for transports built without an injected catalog
// (unit tests and minimal edge paths); module codes resolve as 500.
func PlatformCatalog() *Catalog {
	platformOnce.Do(func() {
		c := NewCatalog()
		c.Seal()
		platformCatalog = c
	})
	return platformCatalog
}

var (
	platformOnce    sync.Once
	platformCatalog *Catalog
)

// Add contributes owner's specs. It fails on an invalid code or spec (status
// outside 400..599, empty title, DocPath without /errors/), on a code already
// owned by another owner or registered with a different spec, and on a sealed
// catalog. Re-adding identical specs for the same owner is a no-op. A failed
// call adds nothing.
func (c *Catalog) Add(provider Provider) error {
	if provider.Owner == "" {
		return fmt.Errorf("apperror: add: owner is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed {
		return fmt.Errorf("apperror: %s: catalog is sealed", provider.Owner)
	}

	pending := make([]Spec, 0, len(provider.Specs))
	seen := make(map[Code]struct{}, len(provider.Specs))
	for _, spec := range provider.Specs {
		code := spec.Code
		if err := validateSpec(code, spec); err != nil {
			return fmt.Errorf("apperror: %s: %w", provider.Owner, err)
		}
		if _, dup := seen[code]; dup {
			return fmt.Errorf("apperror: %s: code %q listed twice", provider.Owner, code)
		}
		seen[code] = struct{}{}
		if existingOwner, exists := c.owners[code]; exists {
			if existingOwner != provider.Owner {
				return fmt.Errorf("apperror: code %q registered by %q is already owned by %q", code, provider.Owner, existingOwner)
			}
			if c.specs[code] != spec {
				return fmt.Errorf("apperror: %s: code %q re-registered with a different spec", provider.Owner, code)
			}
			continue
		}
		pending = append(pending, spec)
	}
	for _, spec := range pending {
		c.specs[spec.Code] = spec
		c.owners[spec.Code] = provider.Owner
		c.order = append(c.order, spec.Code)
	}
	return nil
}

// Seal prevents further additions. Call it once the composition root has
// registered every provider and before serving requests.
func (c *Catalog) Seal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sealed = true
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
func (c *Catalog) SpecFor(code Code) (Spec, bool) {
	if c == nil {
		return Spec{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	spec, ok := c.specs[code]
	return spec, ok
}

// OwnerOf returns the owner that registered code ("platform" for the
// platform-wide catalog) and whether code is known.
func (c *Catalog) OwnerOf(code Code) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	owner, ok := c.owners[code]
	return owner, ok
}

// Codes returns every registered code in stable lexicographic order. The
// returned slice is a fresh copy, so callers may modify it freely.
func (c *Catalog) Codes() []Code {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	codes := make([]Code, 0, len(c.specs))
	for code := range c.specs {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	return codes
}

// Resolve returns the spec that renders code. An unknown code resolves to 500
// system.internal and is logged: a typo must never leak an undefined status or
// title, and the log points at a missing ErrorProvider registration.
func (c *Catalog) Resolve(code Code) (Code, Spec) {
	if spec, ok := c.SpecFor(code); ok {
		return code, spec
	}
	slog.Error("apperror: unregistered error code; responding with system.internal",
		slog.String("code", string(code)))
	if c == nil {
		return CodeSystemInternal, Spec{
			Code:    CodeSystemInternal,
			Status:  http.StatusInternalServerError,
			Title:   "Internal server error",
			DocPath: "/errors/system-internal",
		}
	}
	spec, _ := c.SpecFor(CodeSystemInternal)
	return CodeSystemInternal, spec
}

// Problem renders appErr as the RFC 9457 wire shape resolved through this
// catalog. The error's own Status/Title are presentation carried by the
// constructor; the catalog is authoritative for module codes.
func (c *Catalog) Problem(appErr *AppError, requestID, instance string) ProblemDetails {
	code, spec := c.Resolve(Code(appErr.Code))
	return ProblemDetails{
		Type:      DocsBaseURL + spec.DocPath,
		Title:     spec.Title,
		Status:    spec.Status,
		Code:      string(code),
		Detail:    appErr.Detail,
		Instance:  instance,
		RequestID: requestID,
		Timestamp: timestamp(),
		Errors:    appErr.Errors,
	}
}

// NewCode builds an *AppError from code and public detail. It is a pure
// constructor: it performs no catalog lookup, so the status, title, and type
// are resolved only when the error is rendered through a Catalog. Unknown
// codes therefore render as 500 system.internal at that point.
// detail is public, caller-supplied text for the end user; never pass
// internal causes, SQL, stack traces, or other sensitive data.
func NewCode(code Code, detail string) *AppError {
	return &AppError{Code: string(code), Detail: detail}
}

func platformSpecs() []Spec {
	return []Spec{
		{
			Code:    CodeRequestInvalid,
			Status:  http.StatusBadRequest,
			Title:   "Bad Request",
			DocPath: "/errors/request-invalid",
		},
		{
			Code:    CodeRequestValidationFailed,
			Status:  http.StatusUnprocessableEntity,
			Title:   "Validation failed",
			DocPath: "/errors/request-validation-failed",
		},
		{
			Code:    CodeRequestPayloadTooLarge,
			Status:  http.StatusRequestEntityTooLarge,
			Title:   "Payload Too Large",
			DocPath: "/errors/request-payload-too-large",
		},
		{
			Code:    CodeRequestUnsupportedMediaType,
			Status:  http.StatusUnsupportedMediaType,
			Title:   "Unsupported Media Type",
			DocPath: "/errors/request-unsupported-media-type",
		},
		{
			Code:    CodeRequestMethodNotAllowed,
			Status:  http.StatusMethodNotAllowed,
			Title:   "Method Not Allowed",
			DocPath: "/errors/request-method-not-allowed",
		},
		{
			Code:    CodeRequestNotAcceptable,
			Status:  http.StatusNotAcceptable,
			Title:   "Not Acceptable",
			DocPath: "/errors/request-not-acceptable",
		},
		{
			Code:    CodeRequestCanceled,
			Status:  StatusClientClosedRequest,
			Title:   "Client Closed Request",
			DocPath: "/errors/request-canceled",
		},
		{
			Code:    CodeAuthUnauthenticated,
			Status:  http.StatusUnauthorized,
			Title:   "Unauthorized",
			DocPath: "/errors/auth-unauthenticated",
		},
		{
			Code:    CodePermissionDenied,
			Status:  http.StatusForbidden,
			Title:   "Permission denied",
			DocPath: "/errors/permission-denied",
		},
		{
			Code:    CodeMFARequired,
			Status:  http.StatusForbidden,
			Title:   "Multi-factor authentication required",
			DocPath: "/errors/auth-mfa-required",
		},
		// A disabled feature behaves like an absent route (404), matching the
		// existing "Google OAuth not configured" response; the stable code still
		// lets clients tell it apart from a mistyped URL.
		{
			Code:    CodeFeatureDisabled,
			Status:  http.StatusNotFound,
			Title:   "Feature disabled",
			DocPath: "/errors/feature-disabled",
		},
		{
			Code:      CodeRateLimitExceeded,
			Status:    http.StatusTooManyRequests,
			Title:     "Rate limit exceeded",
			Retryable: true,
			DocPath:   "/errors/rate-limit-exceeded",
		},
		{
			Code:    CodeResourceNotFound,
			Status:  http.StatusNotFound,
			Title:   "Not Found",
			DocPath: "/errors/resource-not-found",
		},
		{
			Code:    CodeResourceConflict,
			Status:  http.StatusConflict,
			Title:   "Conflict",
			DocPath: "/errors/resource-conflict",
		},
		{
			Code:    CodeResourceGone,
			Status:  http.StatusGone,
			Title:   "Gone",
			DocPath: "/errors/resource-gone",
		},
		{
			Code:    CodeResourcePreconditionFailed,
			Status:  http.StatusPreconditionFailed,
			Title:   "Precondition failed",
			DocPath: "/errors/resource-precondition-failed",
		},
		// 422 follows draft-ietf-httpapi-idempotency-key-header: the request is
		// well-formed but cannot be processed under a key bound to another body.
		{
			Code:    CodeIdempotencyKeyMismatch,
			Status:  http.StatusUnprocessableEntity,
			Title:   "Idempotency key reused with a different request",
			DocPath: "/errors/idempotency-key-mismatch",
		},
		{
			Code:    CodeSystemInternal,
			Status:  http.StatusInternalServerError,
			Title:   "Internal server error",
			DocPath: "/errors/system-internal",
		},
		{
			Code:      CodeSystemTimeout,
			Status:    http.StatusGatewayTimeout,
			Title:     "Request timed out",
			Retryable: true,
			DocPath:   "/errors/system-timeout",
		},
		{
			Code:      CodeSystemUnavailable,
			Status:    http.StatusServiceUnavailable,
			Title:     "Service unavailable",
			Retryable: true,
			DocPath:   "/errors/system-unavailable",
		},
		{
			Code:      CodeSystemBadGateway,
			Status:    http.StatusBadGateway,
			Title:     "Bad gateway",
			Retryable: true,
			DocPath:   "/errors/system-bad-gateway",
		},
		{
			Code:      CodeSystemAuditUnavailable,
			Status:    http.StatusServiceUnavailable,
			Title:     "Audit service unavailable",
			Retryable: true,
			DocPath:   "/errors/system-audit-unavailable",
		},
	}
}
