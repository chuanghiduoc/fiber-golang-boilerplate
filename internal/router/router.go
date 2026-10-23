package router

import (
	"crypto/subtle"
	"time"

	"github.com/gofiber/contrib/v3/swagger"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	_ "github.com/chuanghiduoc/fiber-golang-boilerplate/docs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/middleware"
)

func SetupRoutes(app *fiber.App, deps Deps) {
	cfg := deps.Config

	// Uploaded files are served only through the authenticated download
	// endpoints. Do not expose the storage directory as a public static tree:
	// that bypasses ownership checks, security headers, logging, and timeout.

	// Global middleware
	app.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.CORS.Origins(),
		AllowMethods:     cfg.CORS.Methods(),
		AllowHeaders:     cfg.CORS.Headers(),
		AllowCredentials: cfg.CORS.AllowCredentials,
	}))
	app.Use(middleware.SecurityHeaders(cfg.App.Env))
	app.Use(middleware.RequestID())
	app.Use(middleware.Metrics())
	app.Use(middleware.Logger())
	app.Use(middleware.Recovery(cfg.App.Env))
	app.Use(middleware.Timeout(time.Duration(cfg.App.RequestTimeout) * time.Second))

	// Swagger is a development aid; do not expose API docs in production.
	if cfg.App.Env != "production" {
		swaggerHandler := swagger.New(swagger.Config{
			BasePath: "/",
			FilePath: "./docs/swagger.json",
			Path:     "swagger",
		})
		// The contrib UI loads its bundle/styles from unpkg and uses inline
		// initialization, so the strict global CSP must be relaxed for docs.
		swaggerCSP := func(c fiber.Ctx) error {
			c.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self' https://unpkg.com 'unsafe-inline'; style-src 'self' https://unpkg.com 'unsafe-inline'; img-src 'self' data:; font-src 'self' https://unpkg.com; frame-ancestors 'none'")
			return c.Next()
		}
		app.Get("/swagger*", swaggerCSP, swaggerHandler)
		app.Get("/docs/*", swaggerCSP, swaggerHandler)
	}

	// Health check
	app.Get("/healthz", func(c fiber.Ctx) error {
		return c.JSON(deps.Health.Liveness())
	})
	readiness := func(c fiber.Ctx) error {
		st := deps.Health.Readiness(c.Context())
		if st.Status != "up" {
			// 503 lets load balancers drain traffic from a degraded instance.
			return c.Status(fiber.StatusServiceUnavailable).JSON(st)
		}
		return c.JSON(st)
	}
	app.Get("/readyz", readiness)
	// Keep /health as alias for readyz (backward compat)
	app.Get("/health", readiness)

	// Prometheus metrics endpoint. When METRICS_AUTH_TOKEN is set, require a
	// matching Bearer token so internal metrics are not publicly exposed.
	metricsHandler := adaptor.HTTPHandler(promhttp.Handler())
	if token := cfg.App.MetricsAuthToken; token != "" {
		expected := []byte("Bearer " + token)
		app.Get("/metrics", func(c fiber.Ctx) error {
			got := []byte(c.Get(fiber.HeaderAuthorization))
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
			}
			return metricsHandler(c)
		})
	} else {
		app.Get("/metrics", metricsHandler)
	}

	// API v1
	RegisterV1Routes(app.Group("/api/v1"), deps)
}
