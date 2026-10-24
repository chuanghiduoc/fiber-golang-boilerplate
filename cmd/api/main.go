// @title Fiber Golang Boilerplate API
// @version 1.0
// @description REST API boilerplate built with Go Fiber v3, PostgreSQL, and sqlc.
// @basePath /api/v1
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description Enter your bearer token in the format: Bearer {token}
// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name Authorization
// @description Enter an API key in the format: ApiKey {prefix}_{secret}
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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys"
	apikeyadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/adapter"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	apikeyservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/repository"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	notificationsrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationsservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"

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

	// Embedded catalogs are validated at startup so catalog drift or an
	// unsupported I18N_DEFAULT_LOCALE fails fast instead of serving raw keys.
	i18nBundle, err := i18n.New(cfg.I18N.Locale())
	if err != nil {
		slog.Error("failed to initialize i18n", slog.Any("error", err))
		os.Exit(1)
	}

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

	// Tracing is created before the database so pgx can emit sanitized client
	// spans from the first query. The OTLP exporter connects lazily: an
	// unreachable collector does not block or fail startup.
	traceProvider, err := tracing.New(startupCtx, cfg.Tracing)
	if err != nil {
		fail("failed to initialize tracing", err)
	}
	if traceProvider.Enabled() {
		slog.Info("tracing enabled",
			slog.String("exporter", cfg.Tracing.Exporter),
			slog.String("endpoint", cfg.Tracing.Endpoint),
			slog.String("sampler", traceProvider.Sampler()),
		)
	}

	// Email job payloads are encrypted at rest with a key derived from
	// JWT_SECRET. Rotating that secret invalidates already-queued encrypted
	// jobs, so drain the queue before rotating it.
	payloadCodec, err := jobs.NewPayloadCodec(cfg.JWT.Secret)
	if err != nil {
		fail("failed to initialize job payload encryption", err)
	}

	// Create database pool
	poolOpts := make([]database.PoolOption, 0, 1)
	if cfg.Tracing.Enabled && cfg.Tracing.InstrumentDB {
		poolOpts = append(poolOpts, database.WithTracerProvider(traceProvider.TracerProvider()))
	}
	pool, err := database.NewPool(startupCtx, cfg.DB, poolOpts...)
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

	// Feature flags: database defaults/targets are optional. The evaluator
	// serves environment/compiled defaults when the store is unavailable and
	// bounds stale snapshots with FEATURE_FLAGS_MAX_STALENESS_SECONDS.
	flagEvaluator := featureflags.NewEvaluator(featureflags.Options{
		Store:        featureflags.NewPostgresStore(pool),
		Env:          cfg.FeatureFlags.EnvValues(),
		ForceOff:     cfg.FeatureFlags.ForceOffValues(),
		CacheTTL:     cfg.FeatureFlags.CacheTTL(),
		MaxStaleness: cfg.FeatureFlags.MaxStaleness(),
	})
	slog.Info("feature flags initialized", slog.Int("catalog_size", len(featureflags.Catalog())))

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

	// Durable infrastructure. The queue is shared by email, events, and the
	// optional scheduler; one worker processes every registered kind.
	jobQueue := jobs.NewQueue(pool)
	hostname, _ := os.Hostname()
	workerOpts := []jobs.Option{jobs.WithConcurrency(4)}
	if traceProvider.Enabled() {
		workerOpts = append(workerOpts, jobs.WithTracerProvider(traceProvider.TracerProvider()))
	}
	worker := jobs.NewWorker(pool, hostname, workerOpts...)
	eventBus := events.New(jobQueue)

	var scheduleRegistry *scheduler.Registry
	if cfg.Scheduler.Enabled {
		scheduleRegistry = scheduler.NewRegistry()
	}

	// Dependency injection
	userRepo := identityrepository.NewUserRepository(pool)
	tenantRepo := identityrepository.NewTenantRepository(pool)
	tenantResolver := identityadapter.NewTenantResolver(tenantRepo)
	sessionStore := session.NewStore(userRepo, appCache)

	// The baseline seeds tenant id 1. Startup checks existence, not request
	// eligibility: a suspended configured/default tenant must not crash the
	// process. Its requests still return 404 until it is reactivated.
	_, defaultTenant, err := identityrepository.StartupTenants(
		startupCtx, tenantRepo, cfg.Tenant.DefaultSlug,
	)
	if err != nil {
		pool.Close()
		fail("tenant startup check failed", err)
	}
	if defaultTenant.Status != "active" {
		slog.Warn("configured default tenant is suspended; requests for it will return 404 until reactivated",
			slog.String("tenant", defaultTenant.Slug),
			slog.Int64("tenant_id", defaultTenant.ID),
		)
	}
	if cfg.App.Env == "staging" && !cfg.Tenant.RequireHeader {
		slog.Warn("TENANT_REQUIRE_HEADER=false in staging: header-less requests use the default tenant")
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

	// Feature-flag administration shares the evaluator so a successful write
	// invalidates the local snapshot immediately.
	featureRepo := featuresrepo.New(pool)
	featureSvc := featuressvc.New(featureRepo, flagEvaluator)
	featureHandler := featureshandler.NewHandler(featureSvc)

	// Webhooks: tenant-scoped endpoints, durable outbound deliveries, and the
	// public inbound receiver. Secrets are sealed with a dedicated webhook key
	// (JWT_SECRET is the local-only fallback). The HTTP client pins DNS results
	// to vetted public IPs and never follows redirects.
	webhookCodec, err := webhookservice.NewSecretCodec(cfg.Webhook.ResolvedSecretKey(cfg.JWT.Secret))
	if err != nil {
		pool.Close()
		fail("failed to initialize webhook secret encryption", err)
	}
	webhookGuard, err := webhook.NewGuard(cfg.Webhook.SSRFAllowlistEntries())
	if err != nil {
		pool.Close()
		fail("failed to initialize webhook SSRF guard", err)
	}
	webhookClient := webhook.NewClient(webhook.ClientConfig{
		Guard:          webhookGuard,
		Timeout:        cfg.Webhook.RequestTimeout(),
		ConnectTimeout: cfg.Webhook.ConnectTimeout(),
	})
	webhookRepo := webhookrepo.New(pool, jobQueue)
	webhookSvc := webhookservice.New(webhookRepo, webhookRepo, webhookRepo, webhookCodec, auditRecorder, webhookservice.Options{
		DefaultMaxAttempts: cfg.Webhook.MaxAttempts,
		DefaultTimeoutMS:   int32(cfg.Webhook.RequestTimeoutSeconds * 1000), // #nosec G115 -- validated 1..120 seconds
		TimestampTolerance: cfg.Webhook.TimestampTolerance(),
	})
	webhookHandler := webhookshandler.New(webhookSvc)
	webhookDispatcher := webhookservice.NewDispatcher(webhookRepo)

	// Domain events are emitted best-effort after the business mutation commits:
	// file upload/delete and user registration feed tenant-configured webhooks.
	filesvc.AttachWebhookDispatcher(uploadSvc, webhookDispatcher)
	identityservice.AttachWebhookDispatcher(userSvc, webhookDispatcher)
	// API keys: the verifier only needs a hash lookup plus the identity
	// principal adapter; the admin service owns lifecycle and rotation.
	apiKeyRepo := apikeyrepo.NewAPIKeyRepository(pool)
	principalReader := identityadapter.NewPrincipalReader(userRepo)
	apiKeyVerifier := apikey.NewVerifier(apikeyadapter.NewVerifierStore(apiKeyRepo), principalReader)
	apiKeySvc := apikeyservice.NewService(apiKeyRepo, principalReader, txManager)
	apiKeyHandler := apikeyhandler.NewHandler(apiKeySvc)

	// Notifications own their in-app feed and preferences. Identity supplies
	// the email recipient through contracts.NotificationRecipientLookup, and
	// queuedEmail writes the email channel job into the shared outbox.
	notificationRepo := notificationsrepo.NewNotificationRepository(pool)
	notificationSvc := notificationsservice.NewNotificationService(
		notificationRepo,
		identityadapter.NewNotificationRecipient(userRepo),
		queuedEmail,
		txManager,
	)
	notificationHandler := notificationshandler.New(notificationSvc)

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
		Tracing:        traceProvider,
		I18n:           i18nBundle,
		Flags:          flagEvaluator,
		Events:         eventBus,
		Jobs:           worker,
		Schedules:      scheduleRegistry,
		Webhooks:       webhookDispatcher,
		APIKeyVerifier: apiKeyVerifier,
	})
	// Module providers register routes, event subscriptions, job handlers, and
	// schedules. The bus turns durable subscriptions into worker handlers once
	// every module has registered.
	application.Register(
		identity.New(authHandler, userHandler),
		files.New(uploadHandler),
		admin.New(adminHandler),
		features.New(featureHandler),
		webhooks.New(webhookHandler),
		apikeys.New(apiKeyHandler),
		notifications.New(notificationHandler),
	)

	// Durable email worker. It shares the pool but runs outside the request
	// path, so SMTP outages retry instead of failing a registration request.
	emailHandler := jobs.EmailHandler(emailSender, jobs.WithEmailPayloadCodec(payloadCodec))
	// New workers consume both legacy plaintext jobs and encrypted jobs so a
	// rolling deployment can drain the old queue without losing messages.
	worker.Register(jobs.EmailJobKind, emailHandler)
	worker.Register(jobs.EncryptedEmailJobKind, emailHandler)

	// Optional scheduler. It is global (not tenant-scoped); schedules choose a
	// tenant in their job spec, and the worker restores it from jobs.tenant_id.
	var (
		schedulerCancel context.CancelFunc
		schedulerDone   <-chan struct{}
	)
	if cfg.Scheduler.Enabled {
		catchup, err := scheduler.ParseCatchup(cfg.Scheduler.Catchup)
		if err != nil {
			pool.Close()
			fail("invalid scheduler catch-up policy", err)
		}
		sched := scheduler.New(pool, scheduleRegistry, jobQueue, hostname,
			scheduler.WithLeaseTTL(cfg.Scheduler.LeaseTTL()),
			scheduler.WithHeartbeatInterval(cfg.Scheduler.HeartbeatInterval()),
			scheduler.WithPollInterval(cfg.Scheduler.PollInterval()),
			scheduler.WithCatchup(catchup),
			scheduler.WithCatchupLimit(cfg.Scheduler.CatchupLimit),
			scheduler.WithRetention(cfg.Scheduler.Retention()),
		)
		schedulerCtx, cancel := context.WithCancel(context.Background())
		schedulerCancel = cancel
		done := make(chan struct{})
		schedulerDone = done
		go func() {
			defer close(done)
			if err := sched.Run(schedulerCtx); err != nil {
				slog.Error("scheduler stopped", slog.Any("error", err))
			}
		}()
		slog.Info("scheduler enabled", slog.Int("schedules", scheduleRegistry.Len()))
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	webhookWorker := webhookservice.NewDeliveryWorker(webhookRepo, webhookCodec, webhookClient, webhookservice.WorkerOptions{
		RetryBase: cfg.Webhook.RetryBase(),
		RetryMax:  cfg.Webhook.RetryMax(),
	})
	worker.Register(webhookservice.DeliveryJobKind, webhookWorker.Handle)
	workerDone := make(chan struct{})
	go func() {
		if err := worker.Run(workerCtx); err != nil {
			slog.Error("job worker stopped", slog.Any("error", err))
		}
		close(workerDone)
	}()
	// Retention is best-effort and shares the worker lifecycle.
	go webhookservice.RunRetention(workerCtx, webhookSvc, int32(cfg.Webhook.RetentionDays), time.Hour) // #nosec G115 -- validated 1..3650 days

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

	// Stop producing scheduled work first, then stop claiming new jobs and let
	// running handlers drain, then stop HTTP.
	if schedulerCancel != nil {
		schedulerCancel()
		select {
		case <-schedulerDone:
		case <-time.After(10 * time.Second):
			slog.Warn("scheduler did not stop within 10s")
		}
	}
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
	webhookClient.CloseIdleConnections()
	if err := appCache.Close(); err != nil {
		slog.Error("failed to close cache", slog.Any("error", err))
	}
	pool.Close()

	// Flush spans last, after HTTP and the worker have stopped. Export failures
	// (for example a collector outage) are logged but never change the exit
	// code; losing telemetry must not turn a clean shutdown into a failure.
	traceShutdownTimeout := time.Duration(cfg.Tracing.ShutdownTimeout) * time.Second
	if traceShutdownTimeout <= 0 {
		traceShutdownTimeout = 5 * time.Second
	}
	traceShutdownCtx, cancelTrace := context.WithTimeout(context.Background(), traceShutdownTimeout)
	if err := traceProvider.Shutdown(traceShutdownCtx); err != nil {
		slog.Warn("tracing shutdown incomplete; remaining spans were dropped", slog.Any("error", err))
	}
	cancelTrace()

	slog.Info("server exited")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
