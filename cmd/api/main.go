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
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/joho/godotenv/autoload"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"

	_ "github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics" // register Prometheus metrics
)

func main() {
	// Load configuration. Validate checks FEATURE_FLAGS syntax only; unknown
	// keys are rejected below, once modules have registered their flags.
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
		if (cfg.Storage.Driver == "s3" || cfg.Storage.Driver == "seaweedfs") && !cfg.Storage.S3UseSSL {
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

	// Email
	emailSender, err := email.NewSender(cfg.Email)
	if err != nil {
		pool.Close()
		fail("failed to initialize email sender", err)
	}
	slog.Info("email sender initialized", slog.String("driver", cfg.Email.Driver))

	// Transaction manager and the process-wide audit recorder. Tier 1 services
	// write through the transaction; Tier 2 routes use it for intent rows.
	txManager := database.NewTxManager(pool)
	auditRecorder := audit.NewPostgresRecorder(pool)

	// Durable infrastructure. The queue is shared by email, events, and the
	// optional scheduler; one worker processes every registered kind.
	jobQueue := jobs.NewQueue(pool)
	// Hostnames can collide (cloned VMs, host networking, fixed pod names), so
	// the worker and scheduler identity also carries the PID and a random
	// suffix; two processes must never share a lease/lock holder.
	hostname, _ := os.Hostname()
	instanceID := fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), rand.Text()[:8])
	workerOpts := []jobs.Option{jobs.WithConcurrency(4)}
	if traceProvider.Enabled() {
		workerOpts = append(workerOpts, jobs.WithTracerProvider(traceProvider.TracerProvider()))
	}
	worker := jobs.NewWorker(pool, instanceID, workerOpts...)
	eventBus := events.New(jobQueue)

	var scheduleRegistry *scheduler.Registry
	if cfg.Scheduler.Enabled {
		scheduleRegistry = scheduler.NewRegistry()
	}

	// Durable email delivery: services enqueue; the worker sends and retries.
	queuedEmail := jobs.NewQueuedSender(jobQueue, jobs.WithPayloadCodec(payloadCodec))

	// Modules: internal/bootstrap holds the single ordered module list and
	// the contract cross-wiring; each module builds its own repositories,
	// services, and handlers from this shared infrastructure.
	modules, err := bootstrap.Build(app.Infra{
		Config:  cfg,
		Pool:    pool,
		Tx:      txManager,
		Cache:   appCache,
		Audit:   auditRecorder,
		Queue:   jobQueue,
		Email:   queuedEmail,
		Storage: store,
		Flags:   flagEvaluator,
	})
	if err != nil {
		pool.Close()
		fail("failed to build modules", err)
	}

	// Module catalogs (permissions, error codes, flags) must be registered
	// before anything validates against them: FEATURE_FLAGS keys here, the
	// i18n bundle below, and API-key scopes/permission checks per request.
	if err := app.RegisterCatalogs(modules.List...); err != nil {
		pool.Close()
		fail("failed to register module catalogs", err)
	}
	if err := cfg.FeatureFlags.ValidateKnownKeys(); err != nil {
		pool.Close()
		fail("invalid feature flag configuration", err)
	}
	slog.Info("feature flags initialized", slog.Int("catalog_size", len(featureflags.Catalog())))

	// Platform and module catalogs are merged and validated at startup so
	// catalog drift (missing vi keys, placeholder mismatch, key collisions)
	// or an unsupported I18N_DEFAULT_LOCALE fails fast instead of serving
	// raw keys.
	i18nBundle, err := i18n.New(cfg.I18N.Locale(), app.LocaleSources(modules.List...)...)
	if err != nil {
		pool.Close()
		fail("failed to initialize i18n", err)
	}

	// Module startup checks (tenant existence, idempotent admin seed) share
	// the startup deadline.
	if err := modules.Startup(startupCtx); err != nil {
		pool.Close()
		fail("module startup failed", err)
	}
	cancelStartup()

	// Health checker
	healthChecker := health.NewChecker(pool, appCache)

	// Composition root. Modules own their route tables; app owns global
	// middleware, infrastructure routes, and lifecycle.
	application := app.New(cfg, app.Deps{
		Cache:          appCache,
		Sessions:       modules.Sessions,
		Health:         healthChecker,
		Audit:          auditRecorder,
		TenantResolver: modules.TenantResolver,
		Tracing:        traceProvider,
		I18n:           i18nBundle,
		Flags:          flagEvaluator,
		Events:         eventBus,
		Jobs:           worker,
		Schedules:      scheduleRegistry,
		Webhooks:       modules.Webhooks,
		APIKeyVerifier: modules.APIKeyVerifier,
	})
	// Module providers register routes, event subscriptions, job handlers
	// (for example webhook delivery/dispatch), and schedules. The bus turns
	// durable subscriptions into worker handlers once every module has
	// registered.
	application.Register(modules.List...)

	// Durable email worker. email.send is a platform outbox kind: identity
	// and notifications both enqueue it through jobs.QueuedSender and the
	// sender/codec are platform infrastructure, so no module owns the
	// handler. It shares the pool but runs outside the request path, so SMTP
	// outages retry instead of failing a registration request.
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
		sched := scheduler.New(pool, scheduleRegistry, jobQueue, instanceID,
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
	workerDone := make(chan struct{})
	go func() {
		if err := worker.Run(workerCtx); err != nil {
			slog.Error("job worker stopped", slog.Any("error", err))
		}
		close(workerDone)
	}()
	// Module background loops (webhook retention) are best-effort and share
	// the worker lifecycle.
	for _, run := range modules.Background {
		go run(workerCtx)
	}

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

	// One deadline covers every phase so the total stays inside Kubernetes'
	// terminationGracePeriodSeconds (30s, of which preStop uses 5s). Phases
	// share it instead of each taking a fixed 10s.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownBudget)

	// 1. Stop accepting HTTP and drain in-flight requests, concurrently with
	//    the background workers: they are independent (both only need the
	//    pool and cache, which stay open until the end).
	httpDone := make(chan error, 1)
	go func() { httpDone <- application.ShutdownWithContext(shutdownCtx) }()

	// 2. Stop producing scheduled work first, then stop claiming new jobs and
	//    let running handlers drain.
	if schedulerCancel != nil {
		schedulerCancel()
		waitForShutdown(shutdownCtx, schedulerDone, "scheduler")
	}
	workerCancel()
	waitForShutdown(shutdownCtx, workerDone, "job worker")

	if err := <-httpDone; err != nil {
		slog.Error("server forced to shutdown", slog.Any("error", err))
		exitCode = 1
	}

	// 3. HTTP handlers start async.Go work (e.g. post-commit emails); wait for
	//    it only after HTTP has stopped so no new goroutines appear.
	if err := async.Wait(shutdownCtx); err != nil {
		slog.Warn("async goroutines did not finish before the shutdown deadline", slog.Any("error", err))
	}

	// 4. Flush spans after HTTP, workers, and async work have stopped, bounded
	//    by both OTEL_SHUTDOWN_TIMEOUT_SECS and the remaining budget. Export
	//    failures (for example a collector outage) are logged but never change
	//    the exit code; losing telemetry must not fail a clean shutdown.
	traceShutdownTimeout := time.Duration(cfg.Tracing.ShutdownTimeout) * time.Second
	if traceShutdownTimeout <= 0 {
		traceShutdownTimeout = 5 * time.Second
	}
	traceShutdownCtx, cancelTrace := context.WithTimeout(shutdownCtx, traceShutdownTimeout)
	if err := traceProvider.Shutdown(traceShutdownCtx); err != nil {
		slog.Warn("tracing shutdown incomplete; remaining spans were dropped", slog.Any("error", err))
	}
	cancelTrace()
	cancelShutdown()

	// 5. Close shared resources last.
	for _, closeModule := range modules.Close {
		closeModule()
	}
	if err := appCache.Close(); err != nil {
		slog.Error("failed to close cache", slog.Any("error", err))
	}
	pool.Close()

	slog.Info("server exited")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// shutdownBudget is the total graceful-shutdown time after SIGTERM. Kubernetes
// allows terminationGracePeriodSeconds (30s) including the 5s preStop sleep, so
// 20s leaves a 5s margin before SIGKILL. Keep deploy/k8s/base/deployment.yaml in
// sync when changing it.
const shutdownBudget = 20 * time.Second

// waitForShutdown waits for done or the shared shutdown deadline, logging
// which component did not stop in time.
func waitForShutdown(ctx context.Context, done <-chan struct{}, component string) {
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn(component+" did not stop before the shutdown deadline", slog.Duration("budget", shutdownBudget))
	}
}
