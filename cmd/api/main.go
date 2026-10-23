// @title Fiber Golang Boilerplate API
// @version 1.0
// @description REST API boilerplate built with Go Fiber v3, PostgreSQL, and sqlc.
// @basePath /api/v1
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Enter your bearer token in the format: Bearer {token}
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/joho/godotenv/autoload"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/router"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/seed"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"

	_ "github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics" // register Prometheus metrics
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", slog.Any("error", err))
		os.Exit(1)
	}

	// Setup structured logging
	logger.Setup(cfg.App.Env, cfg.App.LogLevel)

	// Base URL for RFC 9457 problem "type" links in error responses.
	apperror.DocsBaseURL = cfg.App.ErrorDocsBaseURL

	if cfg.App.Env == "production" {
		if cfg.Cache.Driver == "memory" {
			slog.Warn("CACHE_DRIVER=memory in production: rate limits and session invalidation are not shared across replicas")
		}
		if (cfg.Storage.Driver == "s3" || cfg.Storage.Driver == "minio") && !cfg.Storage.S3UseSSL {
			slog.Warn("STORAGE_S3_USE_SSL=false in production: object storage traffic is not encrypted")
		}
	}

	if cfg.App.Env != "production" && cfg.App.BodyLimit <= int(cfg.Storage.MaxFileSize) {
		slog.Warn("APP_BODY_LIMIT is smaller than STORAGE_MAX_FILE_SIZE; large uploads will be rejected",
			slog.Int("body_limit", cfg.App.BodyLimit),
			slog.Int64("max_file_size", cfg.Storage.MaxFileSize),
		)
	}

	// Bound startup so an unreachable database cannot hang the process forever.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 60*time.Second)
	fail := func(msg string, err error) {
		cancelStartup()
		slog.Error(msg, slog.Any("error", err))
		os.Exit(1)
	}

	// Create database pool
	pool, err := database.NewPool(startupCtx, cfg.DB)
	if err != nil {
		fail("failed to connect to database", err)
	}

	slog.Info("connected to database")

	// Run migrations
	if err := database.RunMigrations(cfg.DB.DSN(), "migrations"); err != nil {
		pool.Close()
		fail("failed to run migrations", err)
	}
	slog.Info("migrations completed")

	// Initialize storage
	store, err := storage.NewStorage(cfg.Storage)
	if err != nil {
		pool.Close()
		fail("failed to initialize storage", err)
	}
	slog.Info("storage initialized", slog.String("driver", cfg.Storage.Driver))

	// Cache
	appCache, err := cache.NewCache(cfg.Cache)
	if err != nil {
		pool.Close()
		fail("failed to initialize cache", err)
	}
	slog.Info("cache initialized", slog.String("driver", cfg.Cache.Driver))

	// Email
	emailSender, err := email.NewSender(cfg.Email)
	if err != nil {
		pool.Close()
		fail("failed to initialize email sender", err)
	}
	slog.Info("email sender initialized", slog.String("driver", cfg.Email.Driver))

	// Google OAuth (optional)
	var googleOAuth *oauth.GoogleOAuth
	if cfg.OAuth.GoogleClientID != "" {
		googleOAuth = oauth.NewGoogleOAuth(cfg.OAuth)
		if err := googleOAuth.ValidateFrontendURL(); err != nil {
			pool.Close()
			fail("invalid OAuth frontend URL", err)
		}
		slog.Info("Google OAuth enabled")
	}

	// Transaction manager
	txManager := database.NewTxManager(pool)

	// Dependency injection
	userRepo := repository.NewUserRepository(pool)
	sessionStore := session.NewStore(userRepo, appCache)

	// Auto-seed admin user (idempotent)
	if err := seed.Admin(startupCtx, cfg.Admin, userRepo); err != nil {
		pool.Close()
		fail("failed to seed admin user", err)
	}
	cancelStartup()

	refreshTokenRepo := repository.NewRefreshTokenRepository(pool)
	userSvc := service.NewUserService(userRepo, refreshTokenRepo, cfg.App.RequireEmailVerification, appCache, txManager, sessionStore)

	refreshSvc := service.NewRefreshTokenService(refreshTokenRepo, cfg.JWT.RefreshExpireDays)

	// Password reset
	passwordResetRepo := repository.NewPasswordResetRepository(pool)
	passwordResetSvc := service.NewPasswordResetService(
		userRepo, passwordResetRepo, refreshTokenRepo,
		emailSender, appCache, cfg.App.FrontendURL, txManager, sessionStore,
	)

	// Email verification
	emailVerifRepo := repository.NewEmailVerificationRepository(pool)
	emailVerifSvc := service.NewEmailVerificationService(
		userRepo, emailVerifRepo, emailSender, appCache, cfg.App.FrontendURL,
	)

	authHandler := handler.NewAuthHandler(
		userSvc, refreshSvc, passwordResetSvc, emailVerifSvc,
		cfg.JWT.Secret, cfg.JWT.ExpireHour, cfg.App.RequireEmailVerification, googleOAuth,
	)
	userHandler := handler.NewUserHandler(userSvc, emailVerifSvc)

	fileRepo := repository.NewFileRepository(pool)
	uploadSvc := service.NewUploadService(fileRepo, store)
	uploadHandler := handler.NewUploadHandler(uploadSvc, cfg.Storage.MaxFileSize, cfg.Storage.AllowedTypes())

	// Admin
	adminSvc := service.NewAdminService(userRepo, fileRepo, refreshTokenRepo, store, txManager, sessionStore)
	adminHandler := handler.NewAdminHandler(adminSvc)

	// Health checker
	healthChecker := health.NewChecker(pool, appCache)

	// Create Fiber app
	fiberCfg := fiber.Config{
		AppName:      "fiber-golang-boilerplate",
		ErrorHandler: apperror.FiberErrorHandler,
		BodyLimit:    cfg.App.BodyLimit,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	// Behind a reverse proxy, only trust X-Forwarded-* from configured proxies
	// so c.IP() (used for rate limiting) cannot be spoofed by clients.
	if cfg.App.TrustProxy {
		fiberCfg.TrustProxy = true
		fiberCfg.ProxyHeader = fiber.HeaderXForwardedFor
		fiberCfg.TrustProxyConfig = fiber.TrustProxyConfig{Proxies: cfg.App.TrustedProxiesList()}
	}
	app := fiber.New(fiberCfg)

	// Setup routes
	router.SetupRoutes(app, router.Deps{
		AuthHandler:   authHandler,
		UserHandler:   userHandler,
		UploadHandler: uploadHandler,
		AdminHandler:  adminHandler,
		Config:        cfg,
		Pool:          pool,
		Health:        healthChecker,
		Sessions:      sessionStore,
		Cache:         appCache,
	})

	// Graceful shutdown
	serverErr := make(chan error, 1)

	go func() {
		addr := fmt.Sprintf(":%d", cfg.App.Port)
		slog.Info("server starting", slog.String("addr", addr), slog.String("env", cfg.App.Env))
		serverErr <- app.Listen(addr)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	exitCode := 0
	select {
	case listenErr := <-serverErr:
		if listenErr != nil {
			slog.Error("server error", slog.Any("error", listenErr))
			exitCode = 1
		}
	case sig := <-sigChan:
		slog.Info("received signal, shutting down gracefully", slog.String("signal", sig.String()))

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		shutdownErr := app.ShutdownWithContext(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			slog.Error("server forced to shutdown", slog.Any("error", shutdownErr))
			exitCode = 1
		}
	}
	signal.Stop(sigChan)

	// Cleanup all resources
	if err := appCache.Close(); err != nil {
		slog.Error("failed to close cache", slog.Any("error", err))
	}
	pool.Close()

	slog.Info("server exited")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
