// Package httpapi is the single registration path for every HTTP operation.
//
// An operation is declared with an Op (method, path, operationId, tags,
// summary) and a Policy (authentication, permission, MFA, rate-limit tier,
// idempotency, audit, no-store). Register attaches the policy to the huma
// operation metadata, adds the OpenAPI security requirement, and wires the
// request middleware in the only correct order:
//
//	rate limit (IP) -> authenticate -> rate limit (principal) -> authz -> MFA ->
//	idempotency -> audit guard -> handler
//
// The same metadata is what test/arch walks, and cmd/openapi turns the
// registered operations into docs/openapi.yaml without touching a database.
package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Metadata keys attached to every registered huma operation. They are exported
// so test/arch and cmd/openapi can read the same contract the runtime enforces.
const (
	MetadataOp     = "httpapi.op"
	MetadataPolicy = "httpapi.policy"
	// MetadataMiddlewareOrder is the ordered middleware labels the runtime
	// attaches to an operation. test/arch uses it to enforce gate ordering
	// without reflecting over function pointers.
	MetadataMiddlewareOrder = "httpapi.middleware_order"

	metadataMiddlewares = "httpapi.middlewares"
)

// Security scheme names declared in the OpenAPI components.
const (
	SecurityBearer = "bearerAuth"
	SecurityAPIKey = "apiKeyAuth"
	SecurityCookie = "cookieAuth"
)

// RefreshCookieName is the BFF cookie documented by the OpenAPI scheme. The
// identity module owns the cookie itself; the constant keeps the spec and the
// wire contract in one place.
const RefreshCookieName = "__Host-refresh"

// APIVersion is the only public API surface. It is part of every module
// operation path; the version is never negotiated with a header.
const APIVersion = "v1"

// APIPrefix is the mount prefix for public API operations.
const APIPrefix = "/api/" + APIVersion

// Op is the non-policy part of an operation declaration.
type Op struct {
	Method      string
	Path        string
	OperationID string
	Tags        []string
	Summary     string
	Description string
	Deprecated  bool
	// Infrastructure marks a platform probe (health, readiness, metrics) that
	// lives outside /api/v1 and therefore has no tenant resolution.
	Infrastructure bool
	// Errors adds operation-specific error statuses to the OpenAPI document
	// (for example 401 on metrics or 503 on readiness).
	Errors []int
}

// RateLimitTier selects one of the process-wide sliding-window limiters.
type RateLimitTier string

const (
	// RateLimitNone disables per-operation limiting (health probes).
	RateLimitNone RateLimitTier = ""
	// RateLimitStrict is for credential and token endpoints.
	RateLimitStrict RateLimitTier = "strict"
	// RateLimitNormal is the default for authenticated mutations and reads.
	RateLimitNormal RateLimitTier = "normal"
	// RateLimitRelaxed is for cheap read-only lists when another limiter
	// already protects the route.
	RateLimitRelaxed RateLimitTier = "relaxed"
)

// AuditPolicy says how a mutation's audit row is produced.
//
// Exactly one form is used: Tier1Action names the action a service records in
// the mutation transaction, and Guard asks the HTTP layer to write a Tier 2
// intent row. A route with neither is unaudited and test/arch rejects it unless
// it is an explicitly reviewed best-effort case.
type AuditPolicy struct {
	Tier1Action string
	Guard       *GuardPolicy
}

// GuardPolicy configures audithttp.Guard for a Tier 2 mutation.
type GuardPolicy struct {
	Action        string
	Resource      string
	Mode          audithttp.Mode
	RequireDomain bool
}

// Policy is the declarative access and behavior contract for one operation.
type Policy struct {
	// Public marks an explicitly anonymous operation. It is mutually
	// exclusive with Permission/AnyOf and JWTOnly.
	Public bool
	// Permission is the single permission the caller must hold.
	Permission authz.Permission
	// AnyOf grants access when the caller holds at least one permission.
	AnyOf []authz.Permission
	// RequireMFA demands a fresh second factor (amr otp/rc/hwk/swk within
	// MFA_MAX_AGE).
	RequireMFA bool
	// RequirePasskey demands a phishing-resistant passkey proof (amr hwk/swk).
	// It is implied by every platform:* permission, so cross-tenant
	// administration cannot be reached with a phishable TOTP code.
	RequirePasskey bool
	// JWTOnly rejects API-key credentials. Use for session, identity, and
	// destructive self-service operations.
	JWTOnly bool
	// RateLimit selects the limiter tier; empty disables per-operation
	// limiting.
	RateLimit RateLimitTier
	// Feature gates the operation on a catalog flag. It runs after
	// authentication and authorization (so tenant/user targets apply) and
	// before NoStore, Idempotency, and the audit Guard: a disabled feature
	// must not take an idempotency lock or leave an audit intent row.
	Feature features.Key
	// Idempotent enables the Idempotency-Key middleware.
	Idempotent bool
	// CookieAuth documents the BFF refresh cookie as the credential of a
	// public refresh/logout operation.
	CookieAuth bool
	// NoStore sets Cache-Control: no-store on token/secret-returning routes.
	NoStore bool
	// Audit records how the mutation is audited.
	Audit AuditPolicy
}

// Limiters carries the process-wide rate limiters so every operation uses the
// same configured tiers.
type Limiters struct {
	Strict  fiber.Handler
	Normal  fiber.Handler
	Relaxed fiber.Handler
}

func (l Limiters) forTier(tier RateLimitTier) fiber.Handler {
	switch tier {
	case RateLimitNone:
		return nil
	case RateLimitStrict:
		return l.Strict
	case RateLimitNormal:
		return l.Normal
	case RateLimitRelaxed:
		return l.Relaxed
	}
	return nil
}

// Deps is the HTTP foundation's dependency set. app.Deps assembles it; modules
// never construct one.
type Deps struct {
	Limiters        Limiters
	Authenticate    fiber.Handler
	AuthenticateJWT fiber.Handler
	Idempotency     fiber.Handler
	Audit           audit.Recorder
	MFAMaxAge       time.Duration
	I18n            *i18n.Bundle
	Flags           features.Evaluator
	TenantResolver  tenant.Resolver
	DefaultSlug     string
	// ErrorCatalog resolves module error codes when rendering. nil falls back
	// to the platform-only catalog.
	ErrorCatalog *apperror.Catalog
}

// API is the huma API plus the policy registry and runtime dependencies.
type API struct {
	huma huma.API
	deps Deps
	// tenantMiddleware resolves X-Tenant-Slug on /api/v1 operations only.
	tenantMiddleware fiber.Handler
	// catalog resolves error codes for rendering; never nil after New.
	catalogValue *apperror.Catalog

	mu     sync.Mutex
	ops    []OperationInfo
	orders map[string][]string
}

// New builds the huma API over the Fiber app/router, installs the RFC 9457
// error model, and returns the registration facade.
//
// fiberApp is used only for in-process Test calls by huma's adapter. router is
// where operations are mounted: pass the Fiber app so absolute paths (health,
// /api/v1, docs) all live on one router.
func New(fiberApp *fiber.App, router fiber.Router, config huma.Config, deps Deps) *API {
	if fiberApp == nil || router == nil {
		panic("httpapi: New requires a Fiber app and router")
	}
	if deps.Authenticate == nil || deps.AuthenticateJWT == nil {
		panic("httpapi: New requires authentication middleware")
	}
	if deps.Idempotency == nil {
		deps.Idempotency = func(c fiber.Ctx) error { return c.Next() }
	}
	if deps.ErrorCatalog == nil {
		deps.ErrorCatalog = apperror.PlatformCatalog()
	}
	installErrorHooks(deps.ErrorCatalog, deps.I18n)

	adapter := &fiberAdapter{app: fiberApp, router: router}
	humaAPI := huma.NewAPI(config, adapter)
	return &API{
		huma:             humaAPI,
		deps:             deps,
		tenantMiddleware: tenanthttp.Middleware(deps.TenantResolver, deps.DefaultSlug),
		orders:           map[string][]string{},
		catalogValue:     deps.ErrorCatalog,
	}
}

// catalog returns the injected error catalog (never nil).
func (a *API) catalog() *apperror.Catalog {
	if a.catalogValue == nil {
		return apperror.PlatformCatalog()
	}
	return a.catalogValue
}

// DefaultConfig returns the OpenAPI 3.1 configuration shared by the server and
// cmd/openapi. The spec is served at /openapi.json and /openapi.yaml; there is
// no generated UI in this repository.
func DefaultConfig(title, version, description string) huma.Config {
	config := huma.DefaultConfig(title, version)
	// The problem schema is the wire contract; do not add huma's  member
	// or Link headers to responses.
	config.CreateHooks = nil
	config.OpenAPI.OpenAPI = "3.1.0"
	config.Info.Description = description
	config.Servers = []*huma.Server{{URL: "/"}}
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		SecurityBearer: {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "JWT",
			Description:  "Session access token from POST /api/v1/auth/login.",
		},
		SecurityAPIKey: {
			Type:        "apiKey",
			In:          "header",
			Name:        "Authorization",
			Description: "Scoped API key in the form `ApiKey <prefix>_<secret>`.",
		},
		SecurityCookie: {
			Type:        "apiKey",
			In:          "cookie",
			Name:        RefreshCookieName,
			Description: "BFF refresh token (HttpOnly, Secure, SameSite=Strict, Path=/). Cookie-authenticated refresh/logout require the CSRF check described on the operation.",
		},
	}
	config.OpenAPIPath = "/openapi"
	config.DocsPath = ""
	// Prometheus exposition is plain text. Huma only registers JSON by
	// default, so /metrics would otherwise fail content negotiation.
	config.Formats["text/plain"] = huma.Format{Marshal: marshalTextPlain}
	return config
}

// marshalTextPlain writes a string or byte slice as-is. The Prometheus
// exporter returns a named byte-slice type, so reflect is used rather than a
// plain []byte type assertion.
func marshalTextPlain(w io.Writer, v any) error {
	value := reflect.ValueOf(v)
	if value.IsValid() && value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
		_, err := w.Write(value.Bytes())
		return err
	}
	_, err := io.WriteString(w, fmt.Sprint(v))
	return err
}

// Huma returns the underlying API for schema generation and tests.
func (a *API) Huma() huma.API { return a.huma }

// Operations returns every registered module operation in registration order.
func (a *API) Operations() []OperationInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]OperationInfo, len(a.ops))
	copy(out, a.ops)
	return out
}

// OperationInfo pairs an operation declaration with its policy.
type OperationInfo struct {
	Op     Op
	Policy Policy
}

// Register declares one operation and its policy. It panics on a malformed
// declaration (a programming error caught at startup, not a request failure).
func Register[I, O any](api *API, op Op, policy Policy, handler func(context.Context, *I) (*O, error)) {
	if api == nil {
		panic("httpapi: Register requires an API")
	}
	if handler == nil {
		panic("httpapi: Register requires a handler")
	}
	operation := api.prepare(op, policy)
	var zero O
	if status, ok := any(&zero).(interface{ HTTPStatus() int }); ok {
		operation.DefaultStatus = status.HTTPStatus()
	}
	huma.Register(api.huma, operation, func(ctx context.Context, input *I) (*O, error) {
		out, err := handler(ctx, input)
		if err != nil {
			return nil, api.errorFor(ctx, err)
		}
		return out, nil
	})
}

// prepare validates the declaration, records it for introspection, and builds
// the huma operation (metadata, security, documented errors).
func (a *API) prepare(op Op, policy Policy) huma.Operation {
	validateOp(op, policy)
	a.mu.Lock()
	for _, existing := range a.ops {
		if existing.Op.OperationID == op.OperationID {
			a.mu.Unlock()
			panic(fmt.Sprintf("httpapi: duplicate operationId %q", op.OperationID))
		}
		if strings.EqualFold(existing.Op.Method, op.Method) && existing.Op.Path == op.Path {
			a.mu.Unlock()
			panic(fmt.Sprintf("httpapi: duplicate operation %s %s", op.Method, op.Path))
		}
	}
	p := policy
	if platformPermission(p.Permission) || anyPlatformPermission(p.AnyOf) {
		p.RequireMFA = true
		p.RequirePasskey = true
	}
	if p.RequirePasskey {
		p.RequireMFA = true
	}
	a.ops = append(a.ops, OperationInfo{Op: op, Policy: p})
	a.mu.Unlock()

	chain, order := a.middlewareChain(op.Path, p)
	a.orders[op.OperationID] = order
	return huma.Operation{
		Method:      op.Method,
		Path:        op.Path,
		OperationID: op.OperationID,
		Tags:        op.Tags,
		Summary:     op.Summary,
		Description: op.Description,
		Deprecated:  op.Deprecated,
		Metadata: map[string]any{
			MetadataOp:              op,
			MetadataPolicy:          p,
			MetadataMiddlewareOrder: order,
			metadataMiddlewares:     chain,
		},
		Security: securityRequirements(p),
		Errors:   mergeStatuses(documentedErrors(p), op.Errors),
	}
}

// Middleware labels recorded in MetadataMiddlewareOrder. They are the ordering
// contract test/arch enforces.
const (
	MiddlewareTenant       = "tenant"
	MiddlewareRateLimit    = "rate_limit"
	MiddlewareAuthenticate = "authenticate"
	MiddlewareAuthorize    = "authorize"
	MiddlewareMFA          = "mfa"
	MiddlewareFeature      = "feature"
	MiddlewareNoStore      = "no_store"
	MiddlewareIdempotency  = "idempotency"
	MiddlewareAuditGuard   = "audit_guard"
)

// middlewareChain builds the one correct order shared by every operation and
// its labels.
func (a *API) middlewareChain(path string, policy Policy) (handlers []fiber.Handler, order []string) {
	handlers = make([]fiber.Handler, 0, 9)
	order = make([]string, 0, 9)
	add := func(label string, handler fiber.Handler) {
		handlers = append(handlers, handler)
		order = append(order, label)
	}
	if strings.HasPrefix(path, APIPrefix+"/") {
		add(MiddlewareTenant, a.tenantMiddleware)
	}
	limiter := a.deps.Limiters.forTier(policy.RateLimit)
	if limiter != nil {
		// The pre-auth pass bounds a flood of bogus credentials by client IP.
		add(MiddlewareRateLimit, limiter)
	}
	if !policy.Public {
		if policy.JWTOnly {
			add(MiddlewareAuthenticate, a.deps.AuthenticateJWT)
		} else {
			add(MiddlewareAuthenticate, a.deps.Authenticate)
		}
		if limiter != nil {
			// The post-auth pass applies the tier to the principal (user or
			// API key); the limiter key function prefers those locals.
			add(MiddlewareRateLimit, limiter)
		}
	}
	switch {
	case policy.Permission != "":
		add(MiddlewareAuthorize, authzhttp.Require(policy.Permission))
	case len(policy.AnyOf) > 0:
		add(MiddlewareAuthorize, authzhttp.RequireAny(policy.AnyOf...))
	}
	if policy.RequireMFA {
		if policy.RequirePasskey {
			add(MiddlewareMFA, authzhttp.RequirePasskeyMFA(a.deps.MFAMaxAge))
		} else {
			add(MiddlewareMFA, authzhttp.RequireMFA(a.deps.MFAMaxAge))
		}
	}
	if policy.Feature != "" {
		add(MiddlewareFeature, featureGate(a.deps.Flags, policy.Feature))
	}
	if policy.NoStore {
		add(MiddlewareNoStore, middleware.NoStore())
	}
	if policy.Idempotent {
		add(MiddlewareIdempotency, a.deps.Idempotency)
	}
	if policy.Audit.Guard != nil {
		guard := policy.Audit.Guard
		mode := guard.Mode
		if mode == "" {
			mode = audithttp.ModeRequired
		}
		add(MiddlewareAuditGuard, audithttp.Guard(a.deps.Audit, audithttp.Policy{
			Mode:          mode,
			RequireDomain: guard.RequireDomain,
			Action:        guard.Action,
			Resource:      guard.Resource,
		}, a.catalog()))
	}
	return handlers, order
}

// featureGate rejects a disabled flag before any idempotency lock or audit
// intent is taken. The subject carries the request tenant and, when the route
// is authenticated, the caller's user id so user targets apply.
func featureGate(flags features.Evaluator, key features.Key) fiber.Handler {
	return func(c fiber.Ctx) error {
		var userID uuid.UUID
		if subject, ok := authz.SubjectFromContext(c.Context()); ok {
			userID = subject.UserID
		}
		if err := features.Require(c.Context(), flags, key, features.SubjectFromContext(c.Context(), userID)); err != nil {
			return err
		}
		return c.Next()
	}
}

func securityRequirements(policy Policy) []map[string][]string {
	if policy.Public {
		if policy.CookieAuth {
			return []map[string][]string{{SecurityCookie: {}}}
		}
		return nil
	}
	if policy.JWTOnly {
		return []map[string][]string{{SecurityBearer: {}}}
	}
	return []map[string][]string{
		{SecurityBearer: {}},
		{SecurityAPIKey: {}},
	}
}

func documentedErrors(policy Policy) []int {
	codes := []int{http.StatusBadRequest, http.StatusUnprocessableEntity}
	if !policy.Public {
		codes = append(codes, http.StatusUnauthorized)
	}
	if policy.Permission != "" || len(policy.AnyOf) > 0 || policy.RequireMFA {
		codes = append(codes, http.StatusForbidden)
	}
	codes = append(codes, http.StatusNotFound, http.StatusConflict)
	if policy.RateLimit != RateLimitNone {
		codes = append(codes, http.StatusTooManyRequests)
	}
	codes = append(codes, http.StatusInternalServerError)
	return codes
}

func platformPermission(permission authz.Permission) bool {
	return strings.HasPrefix(string(permission), "platform:")
}

func anyPlatformPermission(permissions []authz.Permission) bool {
	return slices.ContainsFunc(permissions, platformPermission)
}

func validateOp(op Op, policy Policy) {
	if op.Method == "" {
		panic("httpapi: op.Method is required")
	}
	switch op.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		panic(fmt.Sprintf("httpapi: unsupported method %q", op.Method))
	}
	if err := ValidatePath(op.Path); err != nil {
		panic(fmt.Sprintf("httpapi: %s %s: %v", op.Method, op.Path, err))
	}
	if strings.TrimSpace(op.OperationID) == "" {
		panic("httpapi: op.OperationID is required")
	}
	if !operationIDPattern.MatchString(op.OperationID) {
		panic(fmt.Sprintf("httpapi: operationId %q must be lowerCamelCase", op.OperationID))
	}
	if len(op.Tags) == 0 {
		panic(fmt.Sprintf("httpapi: %s %s: tags are required", op.Method, op.Path))
	}
	if strings.TrimSpace(op.Summary) == "" {
		panic(fmt.Sprintf("httpapi: %s %s: summary is required", op.Method, op.Path))
	}
	if policy.Public && (policy.Permission != "" || len(policy.AnyOf) > 0 || policy.JWTOnly) {
		panic(fmt.Sprintf("httpapi: %s %s: public operations cannot require a credential or permission", op.Method, op.Path))
	}
	if policy.Permission != "" && len(policy.AnyOf) > 0 {
		panic(fmt.Sprintf("httpapi: %s %s: set Permission or AnyOf, not both", op.Method, op.Path))
	}
	if !policy.Public && policy.Permission == "" && len(policy.AnyOf) == 0 {
		panic(fmt.Sprintf("httpapi: %s %s: every non-public operation needs a permission", op.Method, op.Path))
	}
	if policy.RateLimit != RateLimitNone && policy.RateLimit != RateLimitStrict && policy.RateLimit != RateLimitNormal && policy.RateLimit != RateLimitRelaxed {
		panic(fmt.Sprintf("httpapi: %s %s: unknown rate-limit tier %q", op.Method, op.Path, policy.RateLimit))
	}
	if policy.Audit.Tier1Action != "" && policy.Audit.Guard != nil {
		panic(fmt.Sprintf("httpapi: %s %s: choose Tier1Action or Guard, not both", op.Method, op.Path))
	}
	if !op.Infrastructure && !strings.HasPrefix(op.Path, APIPrefix+"/") {
		panic(fmt.Sprintf("httpapi: %s %s: module operations must live under %s/", op.Method, op.Path, APIPrefix))
	}
}

// Mutating reports whether method changes state. GET is not mutating; the
// Google callback is special-cased by test/arch because it performs a sign-in.
func Mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

var (
	segmentPattern     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	paramPattern       = regexp.MustCompile(`^\{[a-z][a-zA-Z0-9]*\}$`)
	operationIDPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
)

// ValidatePath enforces the canonical path grammar: lowercase kebab-case
// segments, camelCase {pathParameters}, no trailing slash, no colon parameters.
// Custom methods use a final verb segment (for example /files/{id}/restore),
// never /files/{id}:restore, because Fiber treats ':' as a parameter marker.
func ValidatePath(path string) error {
	if path == "" || path[0] != '/' {
		return fmt.Errorf("path must start with /")
	}
	if path != "/" && strings.HasSuffix(path, "/") {
		return fmt.Errorf("path must not end with /")
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("path must not contain //")
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" {
			continue
		}
		if strings.HasPrefix(segment, "{") || strings.HasSuffix(segment, "}") {
			if !paramPattern.MatchString(segment) {
				return fmt.Errorf("parameter %q must look like {camelCaseId}", segment)
			}
			continue
		}
		if strings.Contains(segment, ":") {
			return fmt.Errorf("segment %q uses ':' syntax; use /resource/{id}/verb", segment)
		}
		if !segmentPattern.MatchString(segment) {
			return fmt.Errorf("segment %q must be lowercase kebab-case", segment)
		}
	}
	return nil
}

// mergeStatuses unions status lists without duplicates.
func mergeStatuses(lists ...[]int) []int {
	seen := map[int]bool{}
	var out []int
	for _, list := range lists {
		for _, status := range list {
			if !seen[status] {
				seen[status] = true
				out = append(out, status)
			}
		}
	}
	return out
}

// MiddlewareOrder returns the ordered middleware labels attached to an
// operation. It is the machine-readable contract test/arch enforces.
func (a *API) MiddlewareOrder(operationID string) ([]string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	order, ok := a.orders[operationID]
	out := make([]string, len(order))
	copy(out, order)
	return out, ok
}
