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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
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
	deps.Limiters = buildLimiters(cfg, deps.Cache)
	deps.Idempotency = middleware.Idempotency(deps.Cache)

	fiberCfg := fiber.Config{
		AppName:      "fiber-golang-boilerplate",
		ErrorHandler: apperror.FiberErrorHandler,
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

// Register mounts every module under /api/v1. It is idempotent only in the
// sense that the caller must register before Listen; duplicate paths are a
// programming error caught by Fiber's router.
func (a *App) Register(modules ...Module) {
	v1 := a.fiber.Group("/api/v1")
	for _, m := range modules {
		a.modules = append(a.modules, m)
		m.RegisterRoutes(v1, a.deps)
	}
}

func (a *App) Fiber() *fiber.App { return a.fiber }

func (a *App) Listen(addr string) error { return a.fiber.Listen(addr) }

func (a *App) ShutdownWithContext(ctx context.Context) error {
	return a.fiber.ShutdownWithContext(ctx)
}
