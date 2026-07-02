package router

import (
	"crypto/subtle"
	"time"

	"github.com/gofiber/contrib/v3/swagger"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	_ "github.com/chuanghiduoc/fiber-golang-boilerplate/docs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/middleware"
)

func SetupRoutes(app *fiber.App, deps Deps) {
	cfg := deps.Config

	// Serve local uploads as static files
	if cfg.Storage.Driver == "local" {
		app.Get("/uploads*", static.New(cfg.Storage.LocalPath))
	}

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

	// Swagger
	swaggerHandler := swagger.New(swagger.Config{
		BasePath: "/",
		FilePath: "./docs/swagger.json",
		Path:     "swagger",
	})
	app.Get("/swagger*", swaggerHandler)
	app.Get("/docs/*", swaggerHandler)

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
