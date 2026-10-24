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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

const (
	// startupTimeout bounds startup so an unreachable database cannot hang
	// the process forever.
	startupTimeout = 60 * time.Second
	// workerConcurrency is how many jobs one process runs at once.
	workerConcurrency = 4
	// defaultTraceShutdownTimeout applies when OTEL_SHUTDOWN_TIMEOUT_SECS is
	// not positive (config literals in tests and tools).
	defaultTraceShutdownTimeout = 5 * time.Second
	// instanceSuffixLength is the random suffix length in the instance ID.
	instanceSuffixLength = 8
)

// Slices of config.ShutdownReserve. closeReserve is kept back from the tracing
// flush for closing modules, the cache, and the pool; the minimums apply when
// earlier phases already used their share, and the overrun stays inside the
// 5s margin before SIGKILL.
const (
	closeReserve      = 2 * time.Second
	traceFlushMinimum = time.Second
	closeMinimum      = time.Second
)

// main reads top-down as the process lifecycle: load config, start (build
// infrastructure, register catalogs and modules, run startup checks), start
// background work, serve until a signal or listener error, then shut down
// inside config.ShutdownBudget.
func main() {
	cfg := loadConfig()

	rt, err := start(cfg)
	if err != nil {
		exitStartup(err)
	}

	bg, err := rt.startBackground()
	if err != nil {
		rt.infra.Pool.Close()
		exitStartup(err)
	}

	exitCode := rt.serve()
	if !rt.shutdown(bg) {
		exitCode = 1
	}

	slog.Info("server exited")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// runtime is everything start builds that the later phases run or close.
type runtime struct {
	cfg *config.Config
	// infra is the shared infrastructure handed to every module.
	infra        app.Infra
	trace        *tracing.Provider
	emailSender  email.Sender
	payloadCodec *jobs.PayloadCodec
	worker       *jobs.Worker
	events       *events.Bus
	schedules    *scheduler.Registry // nil unless SCHEDULER_ENABLED
	instanceID   string
	modules      *bootstrap.Modules
	i18n         *i18n.Bundle
	app          *app.App
}

// background holds the stop handles of the goroutines started after startup.
type background struct {
	schedulerCancel context.CancelFunc // nil unless SCHEDULER_ENABLED
	schedulerDone   <-chan struct{}
	workerCancel    context.CancelFunc
	workerDone      <-chan struct{}
}

// startupError carries the log message for the startup step that failed.
type startupError struct {
	msg string
	err error
}

func (e *startupError) Error() string { return e.msg + ": " + e.err.Error() }
func (e *startupError) Unwrap() error { return e.err }

func startupFailed(msg string, err error) error {
	return &startupError{msg: msg, err: err}
}

// exitStartup logs a startup failure and exits with status 1.
func exitStartup(err error) {
	var se *startupError
	if errors.As(err, &se) {
		slog.Error(se.msg, slog.Any("error", se.err))
	} else {
		slog.Error("startup failed", slog.Any("error", err))
	}
	os.Exit(1)
}

// loadConfig loads configuration, sets up logging, and warns about settings
// that are allowed but risky. Validate checks FEATURE_FLAGS syntax only;
// unknown keys are rejected in start, once modules have registered their flags.
func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", slog.Any("error", err))
		os.Exit(1)
	}

	logger.Setup(cfg.App.Env, cfg.App.LogLevel)

	// Base URL for RFC 9457 problem "type" links in error responses.
	apperror.DocsBaseURL = cfg.App.ErrorDocsBaseURL

	warnRiskySettings(cfg)
	return cfg
}

// warnRiskySettings logs configurations that validation accepts on purpose
// (explicit production opt-outs, or local defaults that limit uploads).
func warnRiskySettings(cfg *config.Config) {
	if cfg.App.Env == "production" {
		if cfg.Cache.Driver == "memory" {
			slog.Warn("CACHE_DRIVER=memory in production: rate limits and session invalidation are not shared across replicas")
		}
		if cfg.Storage.UsesS3() && !cfg.Storage.S3UseSSL {
			slog.Warn("STORAGE_S3_ALLOW_INSECURE=true in production: object storage traffic is not encrypted")
		}
	}

	if cfg.App.Env != "production" && cfg.App.BodyLimit <= int(cfg.Storage.MaxFileSize) {
		slog.Warn("APP_BODY_LIMIT is smaller than STORAGE_MAX_FILE_SIZE; large uploads will be rejected",
			slog.Int("body_limit", cfg.App.BodyLimit),
			slog.Int64("max_file_size", cfg.Storage.MaxFileSize),
		)
	}
}

// start builds the process inside the startup deadline: infrastructure, then
// modules and their catalogs, then module startup checks. The HTTP
// application is composed after the deadline is released. On any error after
// the database pool exists, the pool is closed before returning.
func start(cfg *config.Config) (_ *runtime, err error) {
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()

	rt := &runtime{cfg: cfg}
	defer func() {
		if err != nil && rt.infra.Pool != nil {
			rt.infra.Pool.Close()
		}
	}()

	if err := rt.buildInfrastructure(startupCtx); err != nil {
		return nil, err
	}
	if err := rt.buildModules(startupCtx); err != nil {
		return nil, err
	}
	cancelStartup()

	rt.composeApplication()
	return rt, nil
}

// buildInfrastructure creates tracing, the database pool, storage, cache,
// feature flags, email, audit, and the durable job queue/worker/event bus.
func (rt *runtime) buildInfrastructure(ctx context.Context) error {
	cfg := rt.cfg

	// Tracing is created before the database so pgx can emit sanitized client
	// spans from the first query. The OTLP exporter connects lazily: an
	// unreachable collector does not block or fail startup.
	traceProvider, err := tracing.New(ctx, cfg.Tracing)
	if err != nil {
		return startupFailed("failed to initialize tracing", err)
	}
	rt.trace = traceProvider
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
	rt.payloadCodec, err = jobs.NewPayloadCodec(cfg.JWT.Secret)
	if err != nil {
		return startupFailed("failed to initialize job payload encryption", err)
	}

	pool, err := rt.connectDatabase(ctx)
	if err != nil {
		return err
	}

	store, err := storage.NewStorage(cfg.Storage)
	if err != nil {
		return startupFailed("failed to initialize storage", err)
	}
	slog.Info("storage initialized", slog.String("driver", cfg.Storage.Driver))

	appCache, err := cache.NewCache(cfg.Cache)
	if err != nil {
		return startupFailed("failed to initialize cache", err)
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

	rt.emailSender, err = email.NewSender(cfg.Email)
	if err != nil {
		return startupFailed("failed to initialize email sender", err)
	}
	slog.Info("email sender initialized", slog.String("driver", cfg.Email.Driver))

	jobQueue := rt.buildDurableWork(pool)

	rt.infra = app.Infra{
		Config: cfg,
		Pool:   pool,
		// Transaction manager and the process-wide audit recorder. Tier 1
		// services write through the transaction; Tier 2 routes use it for
		// intent rows.
		Tx:      database.NewTxManager(pool),
		Cache:   appCache,
		Audit:   audit.NewPostgresRecorder(pool),
		Queue:   jobQueue,
		Email:   jobs.NewQueuedSender(jobQueue, jobs.WithPayloadCodec(rt.payloadCodec)),
		Storage: store,
		Flags:   flagEvaluator,
	}
	return nil
}

// connectDatabase opens the pool (instrumented when tracing asks for it) and
// records it in rt.infra so start closes it if a later step fails.
func (rt *runtime) connectDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	poolOpts := make([]database.PoolOption, 0, 1)
	if rt.cfg.Tracing.Enabled && rt.cfg.Tracing.InstrumentDB {
		poolOpts = append(poolOpts, database.WithTracerProvider(rt.trace.TracerProvider()))
	}
	pool, err := database.NewPool(ctx, rt.cfg.DB, poolOpts...)
	if err != nil {
		return nil, startupFailed("failed to connect to database", err)
	}
	rt.infra.Pool = pool
	slog.Info("connected to database")

	// Schema management is owned by cmd/migrate only. The API never applies
	// DDL, so replicas can start safely without a migration lock convoy.
	slog.Info("database schema assumed up to date (run cmd/migrate)")
	return pool, nil
}

// buildDurableWork creates the job queue, the worker, the event bus, and the
// optional schedule registry. The queue is shared by email, events, and the
// scheduler; one worker processes every registered kind.
func (rt *runtime) buildDurableWork(pool *pgxpool.Pool) *jobs.Queue {
	jobQueue := jobs.NewQueue(pool)
	rt.instanceID = newInstanceID()
	workerOpts := []jobs.Option{jobs.WithConcurrency(workerConcurrency)}
	if rt.trace.Enabled() {
		workerOpts = append(workerOpts, jobs.WithTracerProvider(rt.trace.TracerProvider()))
	}
	rt.worker = jobs.NewWorker(pool, rt.instanceID, workerOpts...)
	rt.events = events.New(jobQueue)
	if rt.cfg.Scheduler.Enabled {
		rt.schedules = scheduler.NewRegistry()
	}
	return jobQueue
}

// newInstanceID identifies this process as a worker and scheduler lease
// holder. Hostnames can collide (cloned VMs, host networking, fixed pod
// names), so it also carries the PID and a random suffix; two processes must
// never share a lease/lock holder.
func newInstanceID() string {
	hostname, _ := os.Hostname()
	return fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), rand.Text()[:instanceSuffixLength])
}

// buildModules builds every module, registers and validates their catalogs,
// and runs module startup checks inside the startup deadline.
func (rt *runtime) buildModules(ctx context.Context) error {
	// internal/bootstrap holds the single ordered module list and the
	// contract cross-wiring; each module builds its own repositories,
	// services, and handlers from this shared infrastructure.
	modules, err := bootstrap.Build(rt.infra)
	if err != nil {
		return startupFailed("failed to build modules", err)
	}
	rt.modules = modules

	// Module catalogs (permissions, error codes, flags) must be registered
	// before anything validates against them: FEATURE_FLAGS keys here, the
	// i18n bundle below, and API-key scopes/permission checks per request.
	if err := app.RegisterCatalogs(modules.List...); err != nil {
		return startupFailed("failed to register module catalogs", err)
	}
	if err := rt.cfg.FeatureFlags.ValidateKnownKeys(); err != nil {
		return startupFailed("invalid feature flag configuration", err)
	}
	slog.Info("feature flags initialized", slog.Int("catalog_size", len(featureflags.Catalog())))

	// Platform and module catalogs are merged and validated at startup so
	// catalog drift (missing vi keys, placeholder mismatch, key collisions)
	// or an unsupported I18N_DEFAULT_LOCALE fails fast instead of serving
	// raw keys.
	rt.i18n, err = i18n.New(rt.cfg.I18N.Locale(), app.LocaleSources(modules.List...)...)
	if err != nil {
		return startupFailed("failed to initialize i18n", err)
	}

	// Module startup checks (tenant existence, idempotent admin seed) share
	// the startup deadline.
	if err := modules.Startup(ctx); err != nil {
		return startupFailed("module startup failed", err)
	}
	return nil
}

// composeApplication builds the HTTP application, registers module
// providers, and registers the platform email handlers on the worker.
func (rt *runtime) composeApplication() {
	// Composition root. Modules own their route tables; app owns global
	// middleware, infrastructure routes, and lifecycle.
	rt.app = app.New(rt.cfg, app.Deps{
		Cache:          rt.infra.Cache,
		Sessions:       rt.modules.Sessions,
		Health:         health.NewChecker(rt.infra.Pool, rt.infra.Cache),
		Audit:          rt.infra.Audit,
		TenantResolver: rt.modules.TenantResolver,
		Tracing:        rt.trace,
		I18n:           rt.i18n,
		Flags:          rt.infra.Flags,
		Events:         rt.events,
		Jobs:           rt.worker,
		Schedules:      rt.schedules,
		Webhooks:       rt.modules.Webhooks,
		APIKeyVerifier: rt.modules.APIKeyVerifier,
	})
	// Module providers register routes, event subscriptions, job handlers
	// (for example webhook delivery/dispatch), and schedules. The bus turns
	// durable subscriptions into worker handlers once every module has
	// registered.
	rt.app.Register(rt.modules.List...)

	// Durable email worker. email.send is a platform outbox kind: identity
	// and notifications both enqueue it through jobs.QueuedSender and the
	// sender/codec are platform infrastructure, so no module owns the
	// handler. It shares the pool but runs outside the request path, so SMTP
	// outages retry instead of failing a registration request.
	emailHandler := jobs.EmailHandler(rt.emailSender, jobs.WithEmailPayloadCodec(rt.payloadCodec))
	// New workers consume both legacy plaintext jobs and encrypted jobs so a
	// rolling deployment can drain the old queue without losing messages.
	rt.worker.Register(jobs.EmailJobKind, emailHandler)
	rt.worker.Register(jobs.EncryptedEmailJobKind, emailHandler)
}

// startBackground starts the optional scheduler, the job worker, and module
// background loops (webhook retention), which share the worker lifecycle.
func (rt *runtime) startBackground() (background, error) {
	var bg background
	if rt.cfg.Scheduler.Enabled {
		cancel, done, err := rt.startScheduler()
		if err != nil {
			return background{}, err
		}
		bg.schedulerCancel, bg.schedulerDone = cancel, done
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		if err := rt.worker.Run(workerCtx); err != nil {
			slog.Error("job worker stopped", slog.Any("error", err))
		}
		close(workerDone)
	}()
	// Module background loops are best-effort.
	for _, run := range rt.modules.Background {
		go run(workerCtx)
	}
	bg.workerCancel, bg.workerDone = workerCancel, workerDone
	return bg, nil
}

// startScheduler runs the scheduler. It is global (not tenant-scoped);
// schedules choose a tenant in their job spec, and the worker restores it
// from jobs.tenant_id.
func (rt *runtime) startScheduler() (context.CancelFunc, <-chan struct{}, error) {
	cfg := rt.cfg.Scheduler
	catchup, err := scheduler.ParseCatchup(cfg.Catchup)
	if err != nil {
		return nil, nil, startupFailed("invalid scheduler catch-up policy", err)
	}
	sched := scheduler.New(rt.infra.Pool, rt.schedules, rt.infra.Queue, rt.instanceID,
		scheduler.WithLeaseTTL(cfg.LeaseTTL()),
		scheduler.WithHeartbeatInterval(cfg.HeartbeatInterval()),
		scheduler.WithPollInterval(cfg.PollInterval()),
		scheduler.WithCatchup(catchup),
		scheduler.WithCatchupLimit(cfg.CatchupLimit),
		scheduler.WithRetention(cfg.Retention()),
	)
	schedulerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sched.Run(schedulerCtx); err != nil {
			slog.Error("scheduler stopped", slog.Any("error", err))
		}
	}()
	slog.Info("scheduler enabled", slog.Int("schedules", rt.schedules.Len()))
	return cancel, done, nil
}

// serve listens until the listener fails or SIGINT/SIGTERM arrives. It
// returns exit code 1 for a listener error and 0 for a signal.
func (rt *runtime) serve() int {
	serverErr := make(chan error, 1)
	go func() {
		addr := fmt.Sprintf(":%d", rt.cfg.App.Port)
		slog.Info("server starting", slog.String("addr", addr), slog.String("env", rt.cfg.App.Env))
		serverErr <- rt.app.Listen(addr)
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	select {
	case listenErr := <-serverErr:
		if listenErr != nil {
			slog.Error("server error", slog.Any("error", listenErr))
			return 1
		}
	case sig := <-sigChan:
		slog.Info("received signal, shutting down gracefully", slog.String("signal", sig.String()))
	}
	return 0
}

// shutdown stops everything inside config.ShutdownBudget and reports whether
// HTTP drained cleanly. One deadline covers every phase so the total stays
// inside Kubernetes' terminationGracePeriodSeconds (30s, of which preStop
// uses 5s). Draining HTTP, jobs, and async work gets the budget minus
// config.ShutdownReserve; config validation keeps APP_REQUEST_TIMEOUT inside
// that drain window, so the reserve is left for the tracing flush and closing
// shared resources.
func (rt *runtime) shutdown(bg background) bool {
	shutdownDeadline := time.Now().Add(config.ShutdownBudget)
	drainCtx, cancelDrain := context.WithDeadline(context.Background(), shutdownDeadline.Add(-config.ShutdownReserve))
	clean := rt.drain(drainCtx, bg)
	cancelDrain()

	rt.flushTraces(shutdownDeadline)
	rt.closeResources(shutdownDeadline)
	return clean
}

// drain stops HTTP, the scheduler, the worker, and async work within ctx and
// reports whether HTTP shut down cleanly.
func (rt *runtime) drain(ctx context.Context, bg background) bool {
	// 1. Stop accepting HTTP and drain in-flight requests, concurrently with
	//    the background workers: they are independent (both only need the
	//    pool and cache, which stay open until the end).
	httpDone := make(chan error, 1)
	go func() { httpDone <- rt.app.ShutdownWithContext(ctx) }()

	// 2. Stop producing scheduled work first, then stop claiming new jobs and
	//    let running handlers drain.
	if bg.schedulerCancel != nil {
		bg.schedulerCancel()
		waitForShutdown(ctx, bg.schedulerDone, "scheduler")
	}
	bg.workerCancel()
	waitForShutdown(ctx, bg.workerDone, "job worker")

	clean := true
	if err := <-httpDone; err != nil {
		slog.Error("server forced to shutdown", slog.Any("error", err))
		clean = false
	}

	// 3. HTTP handlers start async.Go work (e.g. post-commit emails); wait for
	//    it only after HTTP has stopped so no new goroutines appear.
	if err := async.Wait(ctx); err != nil {
		slog.Warn("async goroutines did not finish before the shutdown deadline", slog.Any("error", err))
	}
	return clean
}

// flushTraces exports buffered spans after HTTP, workers, and async work have
// stopped. The flush gets OTEL_SHUTDOWN_TIMEOUT_SECS capped by what is left of
// the budget after the close reserve, but never less than traceFlushMinimum:
// a drain that used its whole window must not leave tracing with a zero
// deadline. Export failures (for example a collector outage) are logged but
// never change the exit code; losing telemetry must not fail a clean shutdown.
func (rt *runtime) flushTraces(shutdownDeadline time.Time) {
	timeout := time.Duration(rt.cfg.Tracing.ShutdownTimeout) * time.Second
	if timeout <= 0 {
		timeout = defaultTraceShutdownTimeout
	}
	timeout = min(timeout, time.Until(shutdownDeadline)-closeReserve)
	timeout = max(timeout, traceFlushMinimum)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := rt.trace.Shutdown(ctx); err != nil {
		slog.Warn("tracing shutdown incomplete; remaining spans were dropped", slog.Any("error", err))
	}
}

// closeResources closes modules, the cache, and finally the pool, which gets
// what is left of the budget but at least closeMinimum.
func (rt *runtime) closeResources(shutdownDeadline time.Time) {
	for _, closeModule := range rt.modules.Close {
		closeModule()
	}
	if err := rt.infra.Cache.Close(); err != nil {
		slog.Error("failed to close cache", slog.Any("error", err))
	}
	closePool(rt.infra.Pool, max(time.Until(shutdownDeadline), closeMinimum))
}

// closePool closes the database pool without blocking past timeout.
// pgxpool.Close waits for every acquired connection to be released, so a
// handler or job that ignored cancellation would otherwise hold the process
// until Kubernetes sends SIGKILL. On timeout the process exits anyway and the
// server drops the remaining connections.
func closePool(pool *pgxpool.Pool, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("database pool did not close before the shutdown deadline; connections are still checked out",
			slog.Duration("timeout", timeout),
			slog.Int("acquired_conns", int(pool.Stat().AcquiredConns())),
		)
	}
}

// waitForShutdown waits for done or the shared drain deadline, logging which
// component did not stop in time.
func waitForShutdown(ctx context.Context, done <-chan struct{}, component string) {
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn(component+" did not stop before the shutdown deadline",
			slog.Duration("drain_budget", config.ShutdownBudget-config.ShutdownReserve))
	}
}
