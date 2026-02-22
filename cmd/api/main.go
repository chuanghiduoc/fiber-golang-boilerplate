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

	// Create database pool
	ctx := context.Background()
	pool, err := database.NewPool(ctx, cfg.DB)
	if err != nil {
		slog.Error("failed to connect to database", slog.Any("error", err))
		os.Exit(1)
	}

	slog.Info("connected to database")

	// Run migrations
	if err := database.RunMigrations(cfg.DB.DSN(), "migrations"); err != nil {
		pool.Close()
		slog.Error("failed to run migrations", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("migrations completed")

	// Initialize storage
	store, err := storage.NewStorage(cfg.Storage)
	if err != nil {
		pool.Close()
		slog.Error("failed to initialize storage", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("storage initialized", slog.String("driver", cfg.Storage.Driver))

	// Cache
	appCache, err := cache.NewCache(cfg.Cache)
	if err != nil {
		pool.Close()
		slog.Error("failed to initialize cache", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("cache initialized", slog.String("driver", cfg.Cache.Driver))

	// Email
	emailSender, err := email.NewSender(cfg.Email)
	if err != nil {
		pool.Close()
		slog.Error("failed to initialize email sender", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("email sender initialized", slog.String("driver", cfg.Email.Driver))

	// Google OAuth (optional)
	var googleOAuth *oauth.GoogleOAuth
	if cfg.OAuth.GoogleClientID != "" {
		googleOAuth = oauth.NewGoogleOAuth(cfg.OAuth)
		if err := googleOAuth.ValidateFrontendURL(); err != nil {
			slog.Error("invalid OAuth frontend URL", slog.Any("error", err))
			pool.Close()
			os.Exit(1)
		}
		slog.Info("Google OAuth enabled")
	}

	defer pool.Close()

	// Transaction manager
	txManager := database.NewTxManager(pool)

	// Dependency injection
	userRepo := repository.NewUserRepository(pool)

	// Auto-seed admin user (idempotent)
	if err := seed.Admin(ctx, cfg.Admin, userRepo); err != nil {
		slog.Error("failed to seed admin user", slog.Any("error", err))
		return
	}

	refreshTokenRepo := repository.NewRefreshTokenRepository(pool)
	userSvc := service.NewUserService(userRepo, refreshTokenRepo, cfg.App.RequireEmailVerification, appCache, txManager)

	refreshSvc := service.NewRefreshTokenService(refreshTokenRepo, cfg.JWT.RefreshExpireDays)

	// Password reset
	passwordResetRepo := repository.NewPasswordResetRepository(pool)
	passwordResetSvc := service.NewPasswordResetService(
		userRepo, passwordResetRepo, refreshTokenRepo,
		emailSender, appCache, cfg.App.FrontendURL, txManager,
	)

	// Email verification
	emailVerifRepo := repository.NewEmailVerificationRepository(pool)
	emailVerifSvc := service.NewEmailVerificationService(
		userRepo, emailVerifRepo, emailSender, appCache, cfg.App.FrontendURL,
	)

	authHandler := handler.NewAuthHandler(
		userSvc, refreshSvc, passwordResetSvc, emailVerifSvc,
		cfg.JWT.Secret, cfg.JWT.ExpireHour, googleOAuth,
	)
	userHandler := handler.NewUserHandler(userSvc)

	fileRepo := repository.NewFileRepository(pool)
	uploadSvc := service.NewUploadService(fileRepo, store)
	uploadHandler := handler.NewUploadHandler(uploadSvc, cfg.Storage.MaxFileSize, cfg.Storage.AllowedTypes())

	// Admin
	adminSvc := service.NewAdminService(userRepo, fileRepo, refreshTokenRepo, store)
	adminHandler := handler.NewAdminHandler(adminSvc)

	// Health checker
	healthChecker := health.NewChecker(pool, appCache)

	// Create Fiber app
	app := fiber.New(fiber.Config{
		ServerHeader: "fiber-golang-boilerplate",
		AppName:      "fiber-golang-boilerplate",
		ErrorHandler: apperror.FiberErrorHandler,
		BodyLimit:    cfg.App.BodyLimit,
	})

	// Setup routes
	router.SetupRoutes(app, router.Deps{
		AuthHandler:   authHandler,
		UserHandler:   userHandler,
		UploadHandler: uploadHandler,
		AdminHandler:  adminHandler,
		Config:        cfg,
		Pool:          pool,
		Health:        healthChecker,
	})

	// Graceful shutdown
	done := make(chan bool, 1)

	go func() {
		addr := fmt.Sprintf(":%d", cfg.App.Port)
		slog.Info("server starting", slog.String("addr", addr), slog.String("env", cfg.App.Env))
		if err := app.Listen(addr); err != nil {
			slog.Error("server error", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		slog.Info("shutting down gracefully, press Ctrl+C again to force")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := app.ShutdownWithContext(ctx); err != nil {
			slog.Error("server forced to shutdown", slog.Any("error", err))
		}

		_ = appCache.Close()

		done <- true
	}()

	<-done
	slog.Info("server exited")
}
