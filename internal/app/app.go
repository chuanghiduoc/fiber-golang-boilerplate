// Package app is the application composition root. It owns the Fiber instance,
// the huma API, global middleware, infrastructure routes, and the module
// registry. Business capabilities live under internal/modules and register
// their own operations through internal/platform/httpapi.
package app

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey/apikeyhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx/requestctxhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// Module is implemented by every business capability. Modules own their route
// table and must not import another module's internals.
type Module interface {
	Name() string
}

// RouteProvider is the single HTTP registration capability. It is separate from
// Module only so the foundation can land before per-module conversion: a module
// that has not implemented it yet still builds and keeps its catalogs, jobs,
// and events, but mounts no HTTP operations. The conversion removes the
// module's old Fiber RegisterRoutes method.
type RouteProvider interface {
	Module
	RegisterRoutes(api *httpapi.API, deps Deps)
}

// EventProvider is an optional module capability. RegisterEvents runs after
// the module's operations are mounted and before the bus registers durable job
// handlers, so modules may subscribe immediately or durably. A module without
// events simply does not implement it.
type EventProvider interface {
	RegisterEvents(bus *events.Bus)
}

// JobProvider is an optional module capability for durable job handlers. The
// scheduler and event bus use the same worker, so registration is additive.
type JobProvider interface {
	RegisterJobs(registrar jobs.Registrar)
}

// ScheduleProvider is an optional module capability for registering periodic
// schedules. It is only invoked when the scheduler is enabled in the
// composition root; a nil registry is skipped.
type ScheduleProvider interface {
	RegisterSchedules(registry *scheduler.Registry)
}

// PrivacyProvider is an optional module capability for modules holding
// per-user data. RegisterPrivacy runs after routes are mounted and only when
// Deps.Privacy is non-nil.
type PrivacyProvider interface {
	RegisterPrivacy(registry *privacycontracts.Registry)
}

// TenantPurgeProvider is an optional module capability contributing the
// module's tenant-wide purgers. Registration order is the purge order; the
// composition root registers non-identity purgers first and identity last.
type TenantPurgeProvider interface {
	TenantPurgers() []tenancycontracts.Provider
}

// PermissionProvider is an optional module capability declaring the
// permissions the module owns and which built-in roles receive them (admins
// receive every permission implicitly). Permissions checked by more than one
// module stay in the shared authz catalog instead.
type PermissionProvider interface {
	Permissions() []authz.Grant
}

// ErrorProvider is an optional module capability declaring the module's
// stable error codes. Platform-wide codes stay in pkg/apperror.
type ErrorProvider interface {
	Errors() []apperror.Spec
}

// LocaleProvider is an optional module capability supplying en.json/vi.json
// (at the filesystem root) merged into the i18n bundle. Keys are namespaced
// "<module>." or "errors.<code>.".
type LocaleProvider interface {
	Locales() fs.FS
}

// FlagProvider is an optional module capability declaring the module's
// feature flags.
type FlagProvider interface {
	FeatureFlags() []features.Definition
}

// Catalogs are the injected, instance-scoped capability catalogs. The
// composition root creates them once, registers every module provider, seals
// them, and injects them into services, transports, and validation. They are
// never package-level mutable registries.
type Catalogs struct {
	Errors      *apperror.Catalog
	Permissions *authz.Catalog
	Features    *features.Catalog
}

// NewCatalogs returns empty catalogs pre-populated with the platform-owned
// entries. Register adds every module provider and seals the result.
func NewCatalogs() Catalogs {
	return Catalogs{
		Errors:      apperror.NewCatalog(),
		Permissions: authz.NewCatalog(),
		Features:    features.NewCatalog(),
	}
}

// Register adds every module's permissions, error codes, and feature flags,
// owned by Module.Name(), then seals the catalogs. It fails on duplicate module
// names and on any entry another owner already registered. Call it once
// modules are constructed and before anything validates against the catalogs.
func (c Catalogs) Register(modules ...Module) error {
	if c.Errors == nil || c.Permissions == nil || c.Features == nil {
		return fmt.Errorf("app: catalogs are not initialized")
	}
	seen := make(map[string]struct{}, len(modules))
	for _, m := range modules {
		owner := m.Name()
		if owner == "" {
			return fmt.Errorf("app: module %T has an empty name", m)
		}
		if _, dup := seen[owner]; dup {
			return fmt.Errorf("app: module name %q is registered twice", owner)
		}
		seen[owner] = struct{}{}
	}

	for _, m := range modules {
		owner := m.Name()
		if p, ok := m.(PermissionProvider); ok {
			if err := c.Permissions.Add(authz.Provider{Owner: owner, Grants: p.Permissions()}); err != nil {
				return err
			}
		}
		if p, ok := m.(ErrorProvider); ok {
			if err := c.Errors.Add(apperror.Provider{Owner: owner, Specs: p.Errors()}); err != nil {
				return err
			}
		}
		if p, ok := m.(FlagProvider); ok {
			if err := c.Features.Add(features.Provider{Owner: owner, Definitions: p.FeatureFlags()}); err != nil {
				return err
			}
		}
	}
	c.Errors.Seal()
	c.Permissions.Seal()
	c.Features.Seal()
	return nil
}

// BuildCatalogs is the test/composition helper: it creates catalogs from the
// given modules in one call.
func BuildCatalogs(modules ...Module) (Catalogs, error) {
	catalogs := NewCatalogs()
	if err := catalogs.Register(modules...); err != nil {
		return Catalogs{}, err
	}
	return catalogs, nil
}

// LocaleSources returns the module catalogs to pass to i18n.New. Building
// the bundle validates namespaces, collisions, and en/vi parity.
func LocaleSources(modules ...Module) []i18n.Source {
	var sources []i18n.Source
	for _, m := range modules {
		if p, ok := m.(LocaleProvider); ok {
			sources = append(sources, i18n.Source{Owner: m.Name(), FS: p.Locales()})
		}
	}
	return sources
}

// Limiters carries the tiered rate limiters so modules do not rebuild them.
type Limiters = httpapi.Limiters

// ModuleSettings is the process configuration modules may read. It is a
// deliberate subset of config.Config: the HTTP layer needs the JWT keyring and
// lifetime, the MFA freshness bound, and tenant resolution, and nothing else.
type ModuleSettings struct {
	JWT    config.JWTConfig
	MFA    config.MFAConfig
	CORS   config.CORSConfig
	Tenant config.TenantConfig
}

// Deps carries process-wide dependencies shared by modules. Keep this small;
// module-specific dependencies should be captured by the module constructor.
type Deps struct {
	Config      ModuleSettings
	Cache       cache.Cache
	Sessions    session.Store
	Health      *health.Checker
	Limiters    Limiters
	Audit       audit.Recorder
	Idempotency fiber.Handler
	// Tracing is the process tracer provider. Nil means tracing is disabled.
	Tracing *tracing.Provider
	// TenantResolver resolves X-Tenant-Slug (plus the browser auth query
	// fallback) for public routes. A nil resolver is a pass-through for tests
	// and minimal deployments.
	TenantResolver tenant.Resolver
	// I18n negotiates Accept-Language and localizes error responses. Nil
	// disables negotiation and preserves the default English behavior.
	I18n *i18n.Bundle
	// Catalogs are the injected capability catalogs built by the composition
	// root. Error rendering and feature evaluation use them; a zero value falls
	// back to the platform-only catalogs so minimal tests still start.
	Catalogs Catalogs
	// Flags evaluates compile-time feature flags. It is optional for minimal
	// deployments and tests; New replaces nil with an evaluator backed by
	// FEATURE_FLAGS/FEATURE_FLAGS_FORCE_OFF and compiled defaults, so modules
	// never need a nil check.
	Flags features.Evaluator
	// Events is the optional in-process/durable event bus. Nil disables
	// EventProvider registration.
	Events *events.Bus
	// Jobs receives module job handlers when a module implements JobProvider.
	// *jobs.Worker implements it; nil disables JobProvider registration.
	Jobs jobs.Registrar
	// Schedules receives module periodic schedules when the scheduler is
	// enabled. Nil disables ScheduleProvider registration.
	Schedules *scheduler.Registry
	// Privacy is the registry privacy providers register into. Nil disables
	// PrivacyProvider registration (tests and minimal deployments).
	Privacy *privacycontracts.Registry
	// APIKeyVerifier authenticates `Authorization: ApiKey` requests. A nil
	// verifier leaves JWT authentication unchanged; API-key attempts fail 401.
	APIKeyVerifier *apikey.Verifier

	// authGuard is the per-IP pre-authentication limit installed by New. Nil
	// (a Deps literal in tests) skips it.
	authGuard func(fiber.Ctx) error
}

// authLimiterTier namespaces the pre-authentication counters.
const authLimiterTier = "auth"

// Authenticate returns the standard authentication chain for protected module
// operations: `Authorization: ApiKey` is verified by APIKeyVerifier and
// everything else by the JWT/session middleware. A nil verifier keeps JWT
// behavior and rejects API-key attempts with 401. The route permission is
// checked by httpapi after this middleware.
//
// A per-client-IP limit (RATE_LIMIT_AUTH_*) runs first, because the caller
// is unknown until authentication succeeds and the per-operation tier mounted
// ahead of it cannot throttle floods of bogus credentials, each of which would
// otherwise cost an API-key lookup or session check.
func (d Deps) Authenticate() fiber.Handler {
	authenticate := apikeyhttp.Authenticate(middleware.JWTAuthKeys(d.Config.JWT.Keyring(), d.Sessions), d.APIKeyVerifier, d.Catalogs.Permissions)
	guard := d.authGuard
	if guard == nil {
		return authenticate
	}
	return func(c fiber.Ctx) error {
		if err := guard(c); err != nil {
			return err
		}
		return authenticate(c)
	}
}

// AuthenticateJWT is the JWT-only chain for session, identity, and destructive
// self-service operations. It still applies the per-IP auth limiter.
func (d Deps) AuthenticateJWT() fiber.Handler {
	authenticate := middleware.JWTAuthKeys(d.Config.JWT.Keyring(), d.Sessions)
	guard := d.authGuard
	if guard == nil {
		return authenticate
	}
	return func(c fiber.Ctx) error {
		if err := guard(c); err != nil {
			return err
		}
		return authenticate(c)
	}
}

type App struct {
	cfg     *config.Config
	fiber   *fiber.App
	http    *httpapi.API
	deps    Deps
	modules []Module
}

// New builds the Fiber app, the huma API, and global middleware. Operations
// are registered later through Register so the composition root can decide
// which modules are active.
func New(cfg *config.Config, deps Deps) *App {
	deps.Config = ModuleSettings{JWT: cfg.JWT, MFA: cfg.MFA, CORS: cfg.CORS, Tenant: cfg.Tenant}
	if deps.Catalogs.Errors == nil {
		deps.Catalogs.Errors = apperror.NewCatalog()
		deps.Catalogs.Errors.Seal()
	}
	deps.Catalogs.Errors.SetDocsBaseURL(cfg.App.ErrorDocsBaseURL)
	if deps.Catalogs.Permissions == nil {
		deps.Catalogs.Permissions = authz.NewCatalog()
		deps.Catalogs.Permissions.Seal()
	}
	if deps.Catalogs.Features == nil {
		deps.Catalogs.Features = features.NewCatalog()
		deps.Catalogs.Features.Seal()
	}
	if deps.Flags == nil {
		deps.Flags = features.NewEvaluator(features.Options{
			Catalog:      deps.Catalogs.Features,
			Env:          cfg.FeatureFlags.EnvValues(),
			ForceOff:     cfg.FeatureFlags.ForceOffValues(),
			CacheTTL:     cfg.FeatureFlags.EffectiveCacheTTL(),
			MaxStaleness: cfg.FeatureFlags.EffectiveMaxStaleness(),
		})
	}
	if deps.Health == nil {
		deps.Health = health.NewChecker(nil, nil)
	}
	if deps.Audit == nil {
		deps.Audit = audit.Noop{}
	}
	deps.Limiters = buildLimiters(cfg, deps.Cache)
	deps.authGuard = buildAuthGuard(cfg, deps.Cache)
	deps.Idempotency = middleware.Idempotency(deps.Cache)

	fiberCfg := fiber.Config{
		AppName:      "fiber-golang-boilerplate",
		ErrorHandler: apperrorhttp.New(deps.Catalogs.Errors, deps.I18n),
		BodyLimit:    cfg.App.BodyLimit,
		// Stream the request body so a middleware can reject an oversized
		// Content-Length without buffering it, and so the JSON cap is enforced
		// before a handler reads. Multipart parsing stays lazy: only the upload
		// handler parses a form, with APP_BODY_LIMIT as its ceiling.
		StreamRequestBody:            true,
		DisablePreParseMultipartForm: true,
		ReadTimeout:                  60 * time.Second,
		WriteTimeout:                 60 * time.Second,
		IdleTimeout:                  120 * time.Second,
		// Reject malformed/forwarded IP values so X-Forwarded-For cannot
		// create arbitrary rate-limit/idempotency buckets.
		EnableIPValidation: true,
	}
	if cfg.App.TrustProxy {
		fiberCfg.TrustProxy = true
		fiberCfg.ProxyHeader = fiber.HeaderXForwardedFor
		fiberCfg.TrustProxyConfig = fiber.TrustProxyConfig{Proxies: cfg.App.TrustedProxiesList()}
	}

	a := &App{
		cfg:   cfg,
		fiber: fiber.New(fiberCfg),
		deps:  deps,
	}
	a.setupGlobalMiddleware()
	a.setupHTTPAPI()
	a.setupInfrastructureRoutes()
	return a
}

func buildLimiters(cfg *config.Config, appCache cache.Cache) Limiters {
	rl := cfg.RateLimit
	if appCache == nil {
		// Tests and minimal deployments: one process-local counter store shared
		// by all tiers (keys are tier-namespaced).
		appCache = cache.NewMemoryCache()
	}
	return Limiters{
		Strict:  middleware.NewLimiter(rl.StrictMax, int(rl.StrictWindow/time.Second), "strict", appCache),
		Normal:  middleware.NewLimiter(rl.NormalMax, int(rl.NormalWindow/time.Second), "normal", appCache),
		Relaxed: middleware.NewLimiter(rl.RelaxedMax, int(rl.RelaxedWindow/time.Second), "relaxed", appCache),
	}
}

func buildAuthGuard(cfg *config.Config, appCache cache.Cache) func(fiber.Ctx) error {
	maxRequests, windowSecs := cfg.RateLimit.EffectiveAuth()
	// A nil cache falls back to a process-local counter inside NewIPGuard.
	return middleware.NewIPGuard(maxRequests, windowSecs, authLimiterTier, appCache)
}

func (a *App) setupHTTPAPI() {
	defaultSlug := a.cfg.Tenant.DefaultSlug
	if a.cfg.Tenant.RequireHeader {
		defaultSlug = ""
	}
	a.http = httpapi.New(a.fiber, a.fiber,
		httpapi.DefaultConfig(
			"Fiber Golang Boilerplate API",
			"1.0.0",
			"REST API boilerplate built with Go, huma (OpenAPI 3.1), PostgreSQL, and sqlc.",
		),
		httpapi.Deps{
			Limiters:        a.deps.Limiters,
			Authenticate:    a.deps.Authenticate(),
			AuthenticateJWT: a.deps.AuthenticateJWT(),
			Idempotency:     a.deps.Idempotency,
			Audit:           a.deps.Audit,
			MFAMaxAge:       a.cfg.MFA.EffectiveMaxAge(),
			I18n:            a.deps.I18n,
			Flags:           a.deps.Flags,
			ErrorCatalog:    a.deps.Catalogs.Errors,
			TenantResolver:  a.deps.TenantResolver,
			DefaultSlug:     defaultSlug,
			MaxBodyBytes:    int64(a.cfg.App.BodyLimit),
			BodyReadTimeout: a.cfg.App.RequestTimeout,
		},
	)
}

func (a *App) setupGlobalMiddleware() {
	// Outermost safety net: a panic in CORS, request ID, i18n, tracing,
	// metrics, or logging would otherwise crash the process. The inner
	// Recovery below stays where it is so handler panics still flow through
	// Logger/Metrics/Tracing as a 500 with the request ID attached.
	a.fiber.Use(middleware.Recovery(a.cfg.App.Env))
	// RequestID and SecurityHeaders run before CORS: a valid preflight
	// short-circuits inside CORS, and a panic in CORS must still carry the
	// correlation and security headers. Recovery stays outermost.
	a.fiber.Use(middleware.RequestID())
	a.fiber.Use(middleware.SecurityHeaders(a.cfg.App.Env))
	a.fiber.Use(cors.New(cors.Config{
		AllowOrigins:     a.cfg.CORS.Origins(),
		AllowMethods:     a.cfg.CORS.Methods(),
		AllowHeaders:     a.cfg.CORS.Headers(),
		ExposeHeaders:    a.cfg.CORS.ExposedHeaders(),
		AllowCredentials: a.cfg.JWT.RefreshCookie,
	}))
	a.fiber.Use(requestctxhttp.Middleware())
	a.fiber.Use(i18n.Middleware(a.deps.I18n))
	a.fiber.Use(middleware.Tracing(a.deps.Tracing.TracerProvider(), a.cfg.Tracing.ExcludedPaths(), a.cfg.Tracing.TrustIncoming, a.deps.Catalogs.Errors))
	a.fiber.Use(middleware.Metrics(a.deps.Catalogs.Errors))
	a.fiber.Use(middleware.Logger(a.deps.Catalogs.Errors))
	a.fiber.Use(middleware.Recovery(a.cfg.App.Env))
	requestTimeout := a.cfg.App.RequestTimeout
	if drain := config.ShutdownBudget - config.ShutdownReserve; requestTimeout+PostHandlerBudget > drain {
		slog.Warn("APP_REQUEST_TIMEOUT plus post-handler work exceeds the shutdown drain window; in-flight requests may be cut off on shutdown",
			slog.Duration("request_timeout", requestTimeout),
			slog.Duration("post_handler_budget", PostHandlerBudget),
			slog.Duration("drain_window", drain),
		)
	}
	// Timeout wraps body draining as well as auth and the handler. A chunked
	// body without Content-Length is read by BodyLimit before routing, so a
	// timeout placed inside BodyLimit would let a slow client hold the request
	// until Fiber's much larger ReadTimeout.
	a.fiber.Use(middleware.Timeout(requestTimeout, apperrorhttp.New(a.deps.Catalogs.Errors, a.deps.I18n)))
	// Fiber's BodyLimit is the multipart upload ceiling. Every non-multipart
	// body, and multipart bodies on routes outside the upload path, are capped
	// here before auth, rate limiting, or decoding run. The upload route parses
	// its own form lazily under APP_BODY_LIMIT.
	a.fiber.Use(middleware.BodyLimit(
		a.cfg.App.EffectiveJSONBodyLimit(),
		middleware.WithUploadLimit(a.cfg.App.BodyLimit, filesUploadPath),
	))
	a.fiber.Use(audithttp.Middleware(a.deps.Audit))
}

// PostHandlerBudget is the longest a request keeps running after its handler
// returned at the APP_REQUEST_TIMEOUT deadline. Each piece runs on a detached,
// bounded context so it cannot be skipped by the deadline: one audit write
// (Guard completion or the fallback row, never both) and the idempotency
// response store plus lock release. APP_REQUEST_TIMEOUT + PostHandlerBudget
// must fit the shutdown drain window (config.ShutdownBudget -
// config.ShutdownReserve).
const PostHandlerBudget = audit.WriteTimeout + middleware.IdempotencyPostHandlerBudget

// filesUploadPath is the one route allowed to parse a multipart body larger
// than APP_JSON_BODY_LIMIT, up to APP_BODY_LIMIT. Keep it in sync with the
// files module's POST /files route.
const filesUploadPath = "/api/v1/files"

// infrastructureTag groups the platform-owned probe operations in the spec.
const infrastructureTag = "platform"

type healthOutput struct {
	Status int
	Body   health.Status
}

type metricsInput struct {
	Authorization string `header:"Authorization"`
}

// metricsOutput is a raw byte body: huma writes []byte without content
// negotiation, so registering a process-global text/plain format (which would
// also affect every JSON operation) is unnecessary.
type metricsOutput struct {
	ContentType string `header:"Content-Type"`
	Body        []byte
}

// setupInfrastructureRoutes registers the platform-owned operations through the
// same helper modules use. They are outside /api/v1 and therefore have no
// tenant resolution.
func (a *App) setupInfrastructureRoutes() {
	httpapi.Register(a.http, httpapi.Op{
		Method:         http.MethodGet,
		Path:           "/healthz",
		OperationID:    "getLiveness",
		Infrastructure: true,
		Tags:           []string{infrastructureTag},
		Summary:        "Liveness probe",
		Description:    "Reports that the process is running. It checks no dependency.",
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*healthOutput, error) {
		return &healthOutput{Status: http.StatusOK, Body: a.deps.Health.Liveness()}, nil
	})

	readiness := func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		st := a.deps.Health.Readiness(ctx)
		status := http.StatusOK
		if st.Status == health.StatusDown {
			status = http.StatusServiceUnavailable
		}
		return &healthOutput{Status: status, Body: st}, nil
	}
	httpapi.Register(a.http, httpapi.Op{
		Method:         http.MethodGet,
		Path:           "/readyz",
		OperationID:    "getReadiness",
		Infrastructure: true,
		Tags:           []string{infrastructureTag},
		Summary:        "Readiness probe",
		Description:    "Checks the database and cache. Degraded dependencies stay 200; only a down database is 503.",
		Errors:         []int{http.StatusServiceUnavailable},
	}, httpapi.Policy{Public: true}, readiness)
	httpapi.Register(a.http, httpapi.Op{
		Method:         http.MethodGet,
		Path:           "/health",
		OperationID:    "getHealth",
		Infrastructure: true,
		Tags:           []string{infrastructureTag},
		Summary:        "Health probe alias",
		Description:    "Alias of /readyz.",
		Errors:         []int{http.StatusServiceUnavailable},
	}, httpapi.Policy{Public: true}, readiness)

	httpapi.Register(a.http, httpapi.Op{
		Method:              http.MethodGet,
		Path:                "/metrics",
		OperationID:         "getMetrics",
		Infrastructure:      true,
		Tags:                []string{infrastructureTag},
		Summary:             "Prometheus metrics",
		Description:         "Prometheus text exposition. Requires `Authorization: Bearer <METRICS_AUTH_TOKEN>` when a token is configured.",
		Errors:              []int{http.StatusUnauthorized},
		ResponseContentType: "text/plain; version=0.0.4; charset=utf-8",
	}, httpapi.Policy{Public: true}, func(_ context.Context, input *metricsInput) (*metricsOutput, error) {
		if token := a.cfg.App.MetricsAuthToken; token != "" {
			expected := []byte("Bearer " + token)
			if subtle.ConstantTimeCompare([]byte(input.Authorization), expected) != 1 {
				return nil, apperror.NewUnauthorized("unauthorized")
			}
		}
		recorder := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
		if err != nil {
			return nil, apperror.Internal(err, "Internal Server Error")
		}
		metrics.Handler().ServeHTTP(recorder, req)
		return &metricsOutput{
			// Huma negotiates on this exact media type; the exporter may add
			// parameters such as escaping=underscores that Huma rejects.
			ContentType: "text/plain; version=0.0.4; charset=utf-8",
			Body:        recorder.Body.Bytes(),
		}, nil
	})
}

// Register mounts the huma operations of every module that serves HTTP
// (RouteProvider is optional: a contracts-only module has no routes) and runs
// every module's catalog and provider hooks.
//
// Callers must register before Listen; duplicate operations are a programming
// error caught by httpapi.Register.
func (a *App) Register(modules ...Module) {
	for _, m := range modules {
		a.modules = append(a.modules, m)
		if provider, ok := m.(RouteProvider); ok {
			provider.RegisterRoutes(a.http, a.deps)
		}
		a.registerProviders(m)
	}
	a.registerDurableEvents()
}

// RegisterBackground registers module catalogs and the schedule, event, and
// job providers without mounting any API route. APP_ROLE=worker uses it:
// the process serves only the infrastructure routes (health, metrics) while
// its worker runs every module's handlers. Call it instead of Register.
func (a *App) RegisterBackground(modules ...Module) {
	for _, m := range modules {
		a.modules = append(a.modules, m)
		a.registerProviders(m)
	}
	a.registerDurableEvents()
}

// registerProviders invokes m's optional capabilities. They are type
// assertions: a module that only serves routes does not need any
// event/job/schedule plumbing. Nil deps make each hook a no-op for tests and
// minimal deployments.
func (a *App) registerProviders(m Module) {
	if provider, ok := m.(ScheduleProvider); ok && a.deps.Schedules != nil {
		provider.RegisterSchedules(a.deps.Schedules)
	}
	if provider, ok := m.(EventProvider); ok && a.deps.Events != nil {
		provider.RegisterEvents(a.deps.Events)
	}
	if provider, ok := m.(JobProvider); ok && a.deps.Jobs != nil {
		provider.RegisterJobs(systemRegistrar{next: a.deps.Jobs})
	}
	if provider, ok := m.(PrivacyProvider); ok && a.deps.Privacy != nil {
		provider.RegisterPrivacy(a.deps.Privacy)
	}
}

// registerDurableEvents turns durable event subscriptions into worker
// handlers. It runs only after every module has had a chance to subscribe;
// RegisterJobs is idempotent.
func (a *App) registerDurableEvents() {
	if a.deps.Events != nil && a.deps.Jobs != nil {
		if err := a.deps.Events.RegisterJobs(systemRegistrar{next: a.deps.Jobs}); err != nil {
			slog.Warn("failed to register durable event job handlers", slog.Any("error", err))
		}
	}
}

// Fiber returns the underlying Fiber app.
func (a *App) Fiber() *fiber.App { return a.fiber }

// Deps returns the assembled dependencies, for module tests that mount one
// module's operations directly; production code goes through Register.
func (a *App) Deps() Deps { return a.deps }

// HTTP returns the huma operation registry (used by cmd/openapi and test/arch).
func (a *App) HTTP() *httpapi.API { return a.http }

// Huma returns the underlying huma API.
func (a *App) Huma() huma.API { return a.http.Huma() }

func (a *App) Listen(addr string) error { return a.fiber.Listen(addr) }

func (a *App) ShutdownWithContext(ctx context.Context) error {
	return a.fiber.ShutdownWithContext(ctx)
}
