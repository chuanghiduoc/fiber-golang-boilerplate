// Package app is the application composition root. It owns the Fiber instance,
// global middleware, infrastructure routes, and the module registry. Business
// capabilities live under internal/modules and register their own routes.
package app

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/gofiber/contrib/v3/swagger"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey/apikeyhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/versioning"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
)

// Module is implemented by every business capability. Modules own their route
// table and must not import another module's internals.
type Module interface {
	Name() string
	RegisterRoutes(v1 fiber.Router, deps Deps)
}

// V2Module is optionally implemented by modules with a second API surface.
// Modules that do not implement it are mounted only under /api/v1, and the
// /api/v2 group is not created unless at least one module opts in.
type V2Module interface {
	Module
	RegisterV2Routes(v2 fiber.Router, deps Deps)
}

// EventProvider is an optional module capability. RegisterEvents runs after
// the module's routes are mounted and before the bus registers durable job
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
	ErrorCodes() map[apperror.Code]apperror.Spec
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

// RegisterCatalogs registers every module's permissions, error codes, and
// feature flags in the process-wide catalogs, owned by Module.Name(). It
// fails on duplicate module names and on any key another owner already
// registered. Call it once modules are constructed and before anything
// validates against the catalogs: FEATURE_FLAGS key validation, the i18n
// bundle (built from LocaleSources), and serving requests (API-key scope
// validation, permission checks, error rendering). Re-registering the same
// modules is a no-op, so Register can call it again safely.
func RegisterCatalogs(modules ...Module) error {
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

		if p, ok := m.(PermissionProvider); ok {
			if err := authz.Register(owner, p.Permissions()...); err != nil {
				return err
			}
		}
		if p, ok := m.(ErrorProvider); ok {
			if err := apperror.Register(owner, p.ErrorCodes()); err != nil {
				return err
			}
		}
		if p, ok := m.(FlagProvider); ok {
			if err := features.Register(owner, p.FeatureFlags()...); err != nil {
				return err
			}
		}
	}
	return nil
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
type Limiters struct {
	Strict  fiber.Handler
	Normal  fiber.Handler
	Relaxed fiber.Handler
}

// Deps carries process-wide dependencies shared by modules. Keep this small;
// module-specific dependencies should be captured by the module constructor.
type Deps struct {
	Config      *config.Config
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
	// Versioning configures deprecation/sunset metadata for API groups. Nil is
	// equivalent to v1, current, with no deprecation headers.
	Versioning *versioning.Config
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
	// Webhooks publishes domain events to tenant-configured webhook endpoints.
	// Modules depend on the stdlib-only contracts interface; a nil value is
	// replaced with a no-op dispatcher.
	Webhooks webhookcontracts.Dispatcher
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
// routes: `Authorization: ApiKey` is verified by APIKeyVerifier and everything
// else by the JWT/session middleware. A nil verifier keeps JWT behavior and
// rejects API-key attempts with 401. Route permissions still come from
// authzhttp.Require, which sees an API key's scopes as the subject permissions.
//
// A per-client-IP limit (RATE_LIMIT_AUTH_*) runs first, because the caller
// is unknown until authentication succeeds and the per-user tiers mounted
// after it cannot throttle floods of bogus credentials, each of which would
// otherwise cost an API-key lookup or session check.
func (d Deps) Authenticate() fiber.Handler {
	authenticate := apikeyhttp.Authenticate(middleware.JWTAuthKeys(d.Config.JWT.Keyring(), d.Sessions), d.APIKeyVerifier)
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
	deps    Deps
	modules []Module
}

// New builds the Fiber app and global middleware. Routes are registered later
// through Register so the composition root can decide which modules are active.
func New(cfg *config.Config, deps Deps) *App {
	deps.Config = cfg
	if deps.Flags == nil {
		deps.Flags = features.NewEvaluator(features.Options{
			Env:          cfg.FeatureFlags.EnvValues(),
			ForceOff:     cfg.FeatureFlags.ForceOffValues(),
			CacheTTL:     cfg.FeatureFlags.CacheTTL(),
			MaxStaleness: cfg.FeatureFlags.MaxStaleness(),
		})
	}
	deps.Limiters = buildLimiters(cfg, deps.Cache)
	deps.authGuard = buildAuthGuard(cfg, deps.Cache)
	deps.Idempotency = middleware.Idempotency(deps.Cache)
	if deps.Webhooks == nil {
		deps.Webhooks = webhookcontracts.Noop{}
	}

	fiberCfg := fiber.Config{
		AppName:      "fiber-golang-boilerplate",
		ErrorHandler: apperrorhttp.FiberErrorHandlerWithLocalizer(deps.I18n),
		BodyLimit:    cfg.App.BodyLimit,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
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
		Strict:  middleware.NewLimiter(rl.StrictMax, rl.StrictWindow, "strict", appCache),
		Normal:  middleware.NewLimiter(rl.NormalMax, rl.NormalWindow, "normal", appCache),
		Relaxed: middleware.NewLimiter(rl.RelaxedMax, rl.RelaxedWindow, "relaxed", appCache),
	}
}

func buildAuthGuard(cfg *config.Config, appCache cache.Cache) func(fiber.Ctx) error {
	maxRequests, windowSecs := cfg.RateLimit.EffectiveAuth()
	// A nil cache falls back to a process-local counter inside NewIPGuard.
	return middleware.NewIPGuard(maxRequests, windowSecs, authLimiterTier, appCache)
}

func (a *App) setupGlobalMiddleware() {
	// Outermost safety net: a panic in CORS, request ID, i18n, tracing,
	// metrics, or logging would otherwise crash the process. The inner
	// Recovery below stays where it is so handler panics still flow through
	// Logger/Metrics/Tracing as a 500 with the request ID attached.
	a.fiber.Use(middleware.Recovery(a.cfg.App.Env))
	a.fiber.Use(cors.New(cors.Config{
		AllowOrigins:     a.cfg.CORS.Origins(),
		AllowMethods:     a.cfg.CORS.Methods(),
		AllowHeaders:     a.cfg.CORS.Headers(),
		ExposeHeaders:    a.cfg.CORS.ExposedHeaders(),
		AllowCredentials: a.cfg.CORS.AllowCredentials,
	}))
	a.fiber.Use(middleware.SecurityHeaders(a.cfg.App.Env))
	a.fiber.Use(middleware.RequestID())
	a.fiber.Use(i18n.Middleware(a.deps.I18n))
	a.fiber.Use(middleware.Tracing(a.deps.Tracing.TracerProvider(), a.cfg.Tracing.ExcludedPaths(), a.cfg.Tracing.TrustIncoming))
	a.fiber.Use(middleware.Metrics())
	a.fiber.Use(middleware.Logger())
	a.fiber.Use(middleware.Recovery(a.cfg.App.Env))
	// Fiber's BodyLimit is the multipart upload ceiling; every other body is
	// capped here before auth, rate limiting, or decoding run.
	a.fiber.Use(middleware.BodyLimit(a.cfg.App.EffectiveJSONBodyLimit()))
	a.fiber.Use(audithttp.Middleware(a.deps.Audit))
	requestTimeout := time.Duration(a.cfg.App.RequestTimeout) * time.Second
	if drain := config.ShutdownBudget - config.ShutdownReserve; requestTimeout+PostHandlerBudget > drain {
		slog.Warn("APP_REQUEST_TIMEOUT plus post-handler work exceeds the shutdown drain window; in-flight requests may be cut off on shutdown",
			slog.Duration("request_timeout", requestTimeout),
			slog.Duration("post_handler_budget", PostHandlerBudget),
			slog.Duration("drain_window", drain),
		)
	}
	a.fiber.Use(middleware.Timeout(requestTimeout))
}

// PostHandlerBudget is the longest a request keeps running after its handler
// returned at the APP_REQUEST_TIMEOUT deadline. Each piece runs on a detached,
// bounded context so it cannot be skipped by the deadline: one audit write
// (Guard completion or the fallback row, never both) and the idempotency
// response store plus lock release. APP_REQUEST_TIMEOUT + PostHandlerBudget
// must fit the shutdown drain window (config.ShutdownBudget -
// config.ShutdownReserve).
const PostHandlerBudget = audit.WriteTimeout + middleware.IdempotencyPostHandlerBudget

func (a *App) setupInfrastructureRoutes() {
	a.fiber.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(a.deps.Health.Liveness())
	})
	readiness := func(c fiber.Ctx) error {
		st := a.deps.Health.Readiness(c.Context())
		// Degraded (cache down, DB pool saturated) stays 200 so a shared
		// dependency blip does not pull every replica out of rotation at once.
		if st.Status == health.StatusDown {
			return c.Status(fiber.StatusServiceUnavailable).JSON(st)
		}
		return c.JSON(st)
	}
	a.fiber.Get("/readyz", readiness)
	a.fiber.Get("/health", readiness)

	metricsHandler := adaptor.HTTPHandler(promhttp.Handler())
	if token := a.cfg.App.MetricsAuthToken; token != "" {
		expected := []byte("Bearer " + token)
		a.fiber.Get("/metrics", func(c fiber.Ctx) error {
			got := []byte(c.Get(fiber.HeaderAuthorization))
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
			}
			return metricsHandler(c)
		})
	} else {
		a.fiber.Get("/metrics", metricsHandler)
	}

	if a.cfg.App.Env != "production" {
		swaggerHandler := swagger.New(swagger.Config{
			BasePath: "/",
			FilePath: "./docs/swagger.json",
			Path:     "swagger",
		})
		swaggerCSP := func(c fiber.Ctx) error {
			c.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self' https://unpkg.com 'unsafe-inline'; style-src 'self' https://unpkg.com 'unsafe-inline'; img-src 'self' data:; font-src 'self' https://unpkg.com; frame-ancestors 'none'")
			return c.Next()
		}
		a.fiber.Get("/swagger*", swaggerCSP, swaggerHandler)
		a.fiber.Get("/docs/*", swaggerCSP, swaggerHandler)
	}
}

// Register mounts every module under /api/v1. Modules that implement V2Module
// are also mounted under /api/v2; the v2 group is created only when at least
// one module opts in. Callers must register before Listen; duplicate paths are
// a programming error caught by Fiber's router.
//
// Register first (re-)registers the module catalogs. The composition root
// calls RegisterCatalogs earlier to fail with an error and to validate config
// against the complete catalog; this call is then a no-op. A catalog conflict
// reaching this point is a programming error and panics, like a duplicate
// route.
func (a *App) Register(modules ...Module) {
	if err := RegisterCatalogs(modules...); err != nil {
		panic(fmt.Sprintf("app: register module catalogs: %v", err))
	}
	supported := []versioning.Version{versioning.V1}
	hasV2 := false
	for _, m := range modules {
		if _, ok := m.(V2Module); ok {
			hasV2 = true
			break
		}
	}
	if hasV2 {
		supported = append(supported, versioning.V2)
	}
	// Reject /api/v3/... before routing so an unknown version is a 406
	// contract error rather than a generic 404.
	a.fiber.Use("/api", versioning.Guard(supported...))

	// Tenant resolution is API-only: health and metrics probes must not depend
	// on the tenants table. It runs before every module's JWT middleware so a
	// conflicting X-Tenant-Slug can be rejected after the token is parsed. The
	// middleware itself exempts the OAuth callback, whose signed state is the
	// tenant source.
	defaultSlug := a.cfg.Tenant.DefaultSlug
	if a.cfg.Tenant.RequireHeader {
		defaultSlug = ""
	}
	tenantMiddleware := tenanthttp.Middleware(a.deps.TenantResolver, defaultSlug)

	v1 := a.fiber.Group("/api/v1",
		versioning.Middleware(versioning.V1, a.deps.Versioning),
		tenantMiddleware,
	)
	for _, m := range modules {
		a.modules = append(a.modules, m)
		m.RegisterRoutes(v1, a.deps)
		a.registerProviders(m)
	}
	a.registerDurableEvents()

	if !hasV2 {
		return
	}
	v2 := a.fiber.Group("/api/v2",
		versioning.Middleware(versioning.V2, a.deps.Versioning),
		tenantMiddleware,
	)
	for _, m := range modules {
		if v2Module, ok := m.(V2Module); ok {
			v2Module.RegisterV2Routes(v2, a.deps)
		}
	}
}

// RegisterBackground registers module catalogs and the schedule, event, and
// job providers without mounting any /api route. APP_ROLE=worker uses it:
// the process serves only the infrastructure routes (health, metrics) while
// its worker runs every module's handlers. Call it instead of Register.
func (a *App) RegisterBackground(modules ...Module) {
	if err := RegisterCatalogs(modules...); err != nil {
		panic(fmt.Sprintf("app: register module catalogs: %v", err))
	}
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

func (a *App) Fiber() *fiber.App { return a.fiber }

func (a *App) Listen(addr string) error { return a.fiber.Listen(addr) }

func (a *App) ShutdownWithContext(ctx context.Context) error {
	return a.fiber.ShutdownWithContext(ctx)
}
