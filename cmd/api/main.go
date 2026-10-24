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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	adminservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files"
	filesadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepository "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
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

	// Email job payloads are encrypted at rest with a key derived from
	// JWT_SECRET. Rotating that secret invalidates already-queued encrypted
	// jobs, so drain the queue before rotating it.
	payloadCodec, err := jobs.NewPayloadCodec(cfg.JWT.Secret)
	if err != nil {
		fail("failed to initialize job payload encryption", err)
	}

	// Create database pool
	pool, err := database.NewPool(startupCtx, cfg.DB)
	if err != nil {
		fail("failed to connect to database", err)
	}

	slog.Info("connected to database")

	// Schema management is owned by cmd/migrate only. The API never applies
	// DDL, so replicas can start safely without a migration lock convoy.
	slog.Info("database schema assumed up to date (run cmd/migrate)")

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

	// Transaction manager and the process-wide audit recorder. Tier 1 services
	// write through the transaction; Tier 2 routes use it for intent rows.
	txManager := database.NewTxManager(pool)
	auditRecorder := audit.NewPostgresRecorder(pool)

	// Dependency injection
	userRepo := identityrepository.NewUserRepository(pool)
	tenantRepo := identityrepository.NewTenantRepository(pool)
	tenantResolver := identityadapter.NewTenantResolver(tenantRepo)
	sessionStore := session.NewStore(userRepo, appCache)

	// The baseline seeds tenant id 1. Fail fast instead of serving 404s (or
	// creating an admin under an unexpected tenant) when migrations did not run.
	if _, err := tenantRepo.GetByID(startupCtx, 1); err != nil {
		pool.Close()
		fail("default tenant (id 1) is missing; run cmd/migrate", err)
	}
	defaultTenant, err := tenantResolver.Resolve(startupCtx, cfg.Tenant.DefaultSlug)
	if err != nil {
		pool.Close()
		fail("configured default tenant is not available; run cmd/migrate or fix TENANT_DEFAULT_SLUG", err)
	}

	// Auto-seed admin user (idempotent) in the configured default tenant.
	if err := seed.Admin(startupCtx, cfg.Admin, userRepo, defaultTenant.ID); err != nil {
		pool.Close()
		fail("failed to seed admin user", err)
	}
	cancelStartup()

	refreshTokenRepo := identityrepository.NewRefreshTokenRepository(pool)
	userSvc := identityservice.NewUserService(userRepo, refreshTokenRepo, cfg.App.RequireEmailVerification, appCache, txManager, auditRecorder, sessionStore)

	refreshSvc := identityservice.NewRefreshTokenService(refreshTokenRepo, cfg.JWT.RefreshExpireDays)

	// Durable email delivery: services enqueue; the worker sends and retries.
	jobQueue := jobs.NewQueue(pool)
	queuedEmail := jobs.NewQueuedSender(jobQueue, jobs.WithPayloadCodec(payloadCodec))

	// Password reset
	passwordResetRepo := identityrepository.NewPasswordResetRepository(pool)
	passwordResetSvc := identityservice.NewPasswordResetService(
		userRepo, passwordResetRepo, refreshTokenRepo,
		queuedEmail, appCache, cfg.App.FrontendURL, txManager, sessionStore,
	)

	// Email verification
	emailVerifRepo := identityrepository.NewEmailVerificationRepository(pool)
	emailVerifSvc := identityservice.NewEmailVerificationService(
		userRepo, emailVerifRepo, queuedEmail, appCache, cfg.App.FrontendURL, txManager,
	)

	// Registration commits the user, verification token, and email job in one
	// transaction. Resend and email-change paths keep using their own flow.
	userSvc.SetVerificationProvisioner(emailVerifSvc)

	authHandler := identityhandler.NewAuthHandler(
		userSvc, refreshSvc, passwordResetSvc, emailVerifSvc,
		cfg.JWT.Secret, cfg.JWT.ExpireHour, cfg.App.RequireEmailVerification, googleOAuth, tenantRepo,
	)
	userHandler := identityhandler.NewUserHandler(userSvc, emailVerifSvc)

	fileRepo := filesrepo.NewFileRepository(pool)
	uploadSvc := filesvc.NewUploadService(fileRepo, store)
	uploadHandler := fileshandler.NewUploadHandler(uploadSvc, cfg.Storage.MaxFileSize, cfg.Storage.AllowedTypes())

	// Admin composes narrow identity/files contracts; it never imports those
	// modules' repositories, SQL, storage, or session internals.
	identityAdminSvc := identityservice.NewAdminUserService(userRepo, refreshTokenRepo, txManager, auditRecorder, sessionStore)
	filesAdminSvc := filesvc.NewAdminFileService(fileRepo, txManager, auditRecorder, store)
	adminSvc := adminservice.NewService(
		identityadapter.NewAdminIdentity(identityAdminSvc),
		filesadapter.NewAdminFiles(filesAdminSvc),
	)
	adminHandler := adminhandler.NewHandler(adminSvc)

	// Health checker
	healthChecker := health.NewChecker(pool, appCache)

	// Composition root. Modules own their route tables; app owns global
	// middleware, infrastructure routes, and lifecycle.
	application := app.New(cfg, app.Deps{
		Cache:          appCache,
		Sessions:       sessionStore,
		Health:         healthChecker,
		Audit:          auditRecorder,
		TenantResolver: tenantResolver,
	})
	application.Register(
		identity.New(authHandler, userHandler),
		files.New(uploadHandler),
		admin.New(adminHandler),
	)

	// Durable email worker. It shares the pool but runs outside the request
	// path, so SMTP outages retry instead of failing a registration request.
	hostname, _ := os.Hostname()
	workerCtx, workerCancel := context.WithCancel(context.Background())
	worker := jobs.NewWorker(pool, hostname, jobs.WithConcurrency(4))
	emailHandler := jobs.EmailHandler(emailSender, jobs.WithEmailPayloadCodec(payloadCodec))
	// New workers consume both legacy plaintext jobs and encrypted jobs so a
	// rolling deployment can drain the old queue without losing messages.
	worker.Register(jobs.EmailJobKind, emailHandler)
	worker.Register(jobs.EncryptedEmailJobKind, emailHandler)
	workerDone := make(chan struct{})
	go func() {
		if err := worker.Run(workerCtx); err != nil {
			slog.Error("job worker stopped", slog.Any("error", err))
		}
		close(workerDone)
	}()

	// Graceful shutdown
	serverErr := make(chan error, 1)

	go func() {
		addr := fmt.Sprintf(":%d", cfg.App.Port)
		slog.Info("server starting", slog.String("addr", addr), slog.String("env", cfg.App.Env))
		serverErr <- application.Listen(addr)
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
	}
	signal.Stop(sigChan)

	// Stop claiming new jobs, let running handlers drain, then stop HTTP.
	workerCancel()
	select {
	case <-workerDone:
	case <-time.After(10 * time.Second):
		slog.Warn("job worker did not drain within 10s")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	shutdownErr := application.ShutdownWithContext(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		slog.Error("server forced to shutdown", slog.Any("error", shutdownErr))
		exitCode = 1
	}

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
