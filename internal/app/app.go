// Package app is the application composition root. It owns the Fiber instance,
// global middleware, infrastructure routes, and the module registry. Business
// capabilities live under internal/modules and register their own routes.
package app

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/gofiber/contrib/v3/swagger"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/versioning"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
	deps.Idempotency = middleware.Idempotency(deps.Cache)

	fiberCfg := fiber.Config{
		AppName:      "fiber-golang-boilerplate",
		ErrorHandler: apperror.FiberErrorHandlerWithLocalizer(deps.I18n),
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
	storage := middleware.NewLimiterStorage(appCache)
	rl := cfg.RateLimit
	return Limiters{
		Strict:  middleware.NewLimiter(rl.StrictMax, rl.StrictWindow, "strict", storage),
		Normal:  middleware.NewLimiter(rl.NormalMax, rl.NormalWindow, "normal", storage),
		Relaxed: middleware.NewLimiter(rl.RelaxedMax, rl.RelaxedWindow, "relaxed", storage),
	}
}

func (a *App) setupGlobalMiddleware() {
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
	a.fiber.Use(middleware.Tracing(a.deps.Tracing.TracerProvider(), a.cfg.Tracing.ExcludedPaths()))
	a.fiber.Use(middleware.Metrics())
	a.fiber.Use(middleware.Logger())
	a.fiber.Use(middleware.Recovery(a.cfg.App.Env))
	a.fiber.Use(audit.Middleware(a.deps.Audit))
	a.fiber.Use(middleware.Timeout(time.Duration(a.cfg.App.RequestTimeout) * time.Second))
}

func (a *App) setupInfrastructureRoutes() {
	a.fiber.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(a.deps.Health.Liveness())
	})
	readiness := func(c fiber.Ctx) error {
		st := a.deps.Health.Readiness(c.Context())
		if st.Status != "up" {
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
func (a *App) Register(modules ...Module) {
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
	tenantMiddleware := tenant.Middleware(a.deps.TenantResolver, defaultSlug)

	v1 := a.fiber.Group("/api/v1",
		versioning.Middleware(versioning.V1, a.deps.Versioning),
		tenantMiddleware,
	)
	for _, m := range modules {
		a.modules = append(a.modules, m)
		m.RegisterRoutes(v1, a.deps)
	}

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

func (a *App) Fiber() *fiber.App { return a.fiber }

func (a *App) Listen(addr string) error { return a.fiber.Listen(addr) }

func (a *App) ShutdownWithContext(ctx context.Context) error {
	return a.fiber.ShutdownWithContext(ctx)
}
