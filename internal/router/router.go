package router

import (
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
	app.Get("/readyz", func(c fiber.Ctx) error {
		return c.JSON(deps.Health.Readiness(c.Context()))
	})
	// Keep /health as alias for readyz (backward compat)
	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(deps.Health.Readiness(c.Context()))
	})

	// Prometheus metrics endpoint
	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	// API v1
	RegisterV1Routes(app.Group("/api/v1"), deps)
}
