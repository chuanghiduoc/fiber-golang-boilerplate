package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/logging"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/storage/drivers"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

const (
	// startupTimeout bounds startup so an unreachable database cannot hang
	// the process forever.
	startupTimeout = 60 * time.Second
	// workerConcurrency is how many jobs one process runs at once.
	workerConcurrency = 4
	// defaultTraceShutdownTimeout applies when TRACING_SHUTDOWN_TIMEOUT is
	// not positive (config literals in tests and tools).
	defaultTraceShutdownTimeout = 5 * time.Second
	// defaultMetricsShutdownTimeout bounds the OTLP metric flush.
	defaultMetricsShutdownTimeout = 5 * time.Second
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

// version is the release tag or commit SHA, injected at build time with
// -ldflags "-X main.version=<tag>". The Docker image sets it from its VERSION
// build argument; a local `go build` keeps the "dev" fallback.
var version = "dev"

// main reads top-down as the process lifecycle: run the container health check
// when invoked as a subcommand, otherwise load config, start (build
// infrastructure, register catalogs and modules, run startup checks), start
// background work, serve until a signal or listener error, then shut down
// inside config.ShutdownBudget.
// main runs the container health check subcommand or the server lifecycle.
// run returns an exit code so deferred cleanup in the normal path executes
// before the process exits.
func main() {
	// The distroless runtime has no shell, curl, or wget, so the container
	// health check is this binary's own subcommand. Handle it before loading
	// configuration: a probe must not depend on the deployment's secrets.
	if len(os.Args) > 1 && os.Args[1] == healthcheckCommand {
		os.Exit(runHealthcheck(os.Args[2:], os.Stdout, os.Stderr, os.Getenv))
	}
	os.Exit(run())
}

// run executes one server lifecycle and returns its process exit code.
func run() int {
	// Register signal handling before startup. A SIGTERM during a slow database
	// or storage initialization must cancel startup and run cleanup instead of
	// using the default action and bypassing the shutdown path.
	shutdownCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("failed to load config", slog.Any("error", err))
		return 1
	}
	slog.Info("build info", slog.String("version", version))

	rt, err := start(shutdownCtx, cfg)
	if err != nil {
		logStartupFailure(err)
		return 1
	}

	bg, err := rt.startBackground()
	if err != nil {
		rt.closeDatabasePools()
		logStartupFailure(err)
		return 1
	}

	exitCode := rt.serve(shutdownCtx)
	if !rt.shutdown(bg) {
		exitCode = 1
	}

	slog.Info("server exited")
	return exitCode
}

// runtime is everything start builds that the later phases run or close.
type runtime struct {
	cfg *config.Config
	// infra is the shared infrastructure handed to every module.
	infra app.Infra
	// pools are private to this command: modules only receive the scoped DB
	// handle, never a raw pool.
	pools        database.Pools
	trace        *tracing.Provider
	metrics      *metrics.Provider
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
	workerCancel    context.CancelFunc // nil for APP_ROLE=api
	workerDone      <-chan struct{}
	// loopsDone is closed when every module background loop returned. It is
	// nil for APP_ROLE=api, which starts none.
	loopsDone <-chan struct{}
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

// logStartupFailure logs a startup failure; the caller returns exit status 1.
func logStartupFailure(err error) {
	var se *startupError
	if errors.As(err, &se) {
		slog.Error(se.msg, slog.Any("error", se.err))
	} else {
		slog.Error("startup failed", slog.Any("error", err))
	}
}

// loadConfig loads configuration, sets up logging, and warns about settings
// that are allowed but risky. Validate checks FEATURE_FLAGS syntax only;
// unknown keys are rejected in start, once modules have registered their flags.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	logger.SetupWith(cfg.App.Env, cfg.App.LogLevel, logging.Wrap)

	// The optional breached-password check is process-wide: the validator tag
	// runs deep inside DTO binding, so the checker is installed once here.
	if cfg.Password.HIBPEnabled() {
		validator.SetBreachChecker(validator.NewHIBPChecker(2 * time.Second))
		slog.Info("breached-password check enabled", slog.String("provider", "hibp"))
	} else {
		validator.SetBreachChecker(nil)
	}

	warnRiskySettings(cfg)
	return cfg, nil
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
		if cfg.App.RunsWorker() && !cfg.Scheduler.Enabled {
			slog.Warn("SCHEDULER_ENABLED=false in production: scheduled jobs and data-retention purges will never run in this deployment",
				slog.String("app_role", cfg.App.EffectiveRole()),
			)
		}
		if cfg.App.ServesAPI() && cfg.Files.EnabledScanDriver() == "none" {
			slog.Warn("FILES_SCAN_DRIVER=none in production: uploaded files are not scanned for malware")
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
func start(ctx context.Context, cfg *config.Config) (_ *runtime, err error) {
	startupCtx, cancelStartup := context.WithTimeout(ctx, startupTimeout)
	defer cancelStartup()

	rt := &runtime{cfg: cfg}
	defer func() {
		if err != nil {
			rt.closeDatabasePools()
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
	traceProvider, err := tracing.New(ctx, cfg.Tracing, tracing.WithServiceVersion(version))
	if err != nil {
		return startupFailed("failed to initialize tracing", err)
	}
	rt.trace = traceProvider
	if traceProvider.Enabled() {
		slog.Info("tracing enabled",
			slog.String("exporter", traceProvider.Exporter()),
			slog.String("sampler", traceProvider.Sampler()),
		)
	}

	// Metrics are set up before the database and HTTP server so every
	// instrument created after this point records into the final provider. The
	// Prometheus scrapes stay on /metrics; OTLP is additive when enabled.
	metricProvider, err := metrics.Setup(metrics.Options{
		ServiceVersion: version,
		OTLPEnabled:    cfg.Metrics.OTLPEnabled,
	})
	if err != nil {
		return startupFailed("failed to initialize metrics", err)
	}
	rt.metrics = metricProvider

	// Email and password-reset job payloads are encrypted at rest with
	// the dedicated DATA_ENCRYPTION_KEY (no JWT_SECRET fallback). Envelopes
	// record their key id, so a rotation keeps the old key in
	// DATA_ENCRYPTION_PREVIOUS_KEYS until queued jobs sealed with it drain.
	rt.payloadCodec, err = jobs.NewPayloadCodec(
		cfg.DataEncryption.ResolvedKey(), cfg.DataEncryption.PreviousKeyList()...)
	if err != nil {
		return startupFailed("failed to initialize job payload encryption", err)
	}

	pools, err := rt.connectDatabase(ctx)
	if err != nil {
		return err
	}
	if err := checkRLSRole(ctx, pools.Primary, cfg.DB.Schema, cfg.App.Env); err != nil {
		return err
	}
	if err := checkBypassRLSRole(ctx, pools.Bypass, pools.Primary, cfg.DB.Schema, cfg.App.Env); err != nil {
		return err
	}

	store, err := drivers.New(cfg.Storage)
	if err != nil {
		return startupFailed("failed to initialize storage", err)
	}
	slog.Info("storage initialized", slog.String("driver", cfg.Storage.Driver))

	appCache, err := cache.NewCache(cfg.Cache)
	if err != nil {
		return startupFailed("failed to initialize cache", err)
	}
	slog.Info("cache initialized", slog.String("driver", cfg.Cache.Driver))

	// Every statement runs with the tenant's RLS scope; the resolver is
	// injected here because pkg/database cannot import internal packages.
	scoped := database.NewScopedPools(pools, database.WithResolver(app.RLSResolver))

	// Capability catalogs are created before every module so services can
	// receive the same instances; Register adds the module providers after
	// Build and seals them before serving.
	catalogs := app.NewCatalogs()

	// Feature flags: database defaults/targets are optional. The evaluator
	// serves environment/compiled defaults when the store is unavailable and
	// bounds stale snapshots with FEATURE_FLAGS_MAX_STALENESS. The
	// snapshot covers every tenant, so its reads use the bypass pool.
	flagStoreDB := database.NewScopedPools(pools, database.WithResolver(func(context.Context) database.Scope {
		return database.Scope{Bypass: true}
	}))
	flagEvaluator := featureflags.NewEvaluator(featureflags.Options{
		Store:        featureflags.NewPostgresStore(flagStoreDB),
		Catalog:      catalogs.Features,
		Env:          cfg.FeatureFlags.EnvValues(),
		ForceOff:     cfg.FeatureFlags.ForceOffValues(),
		CacheTTL:     cfg.FeatureFlags.EffectiveCacheTTL(),
		MaxStaleness: cfg.FeatureFlags.EffectiveMaxStaleness(),
	})

	rt.emailSender, err = email.NewSender(cfg.Email)
	if err != nil {
		return startupFailed("failed to initialize email sender", err)
	}
	slog.Info("email sender initialized", slog.String("driver", cfg.Email.Driver))

	jobQueue := rt.buildDurableWork(pools.Primary, scoped)

	rt.infra = app.Infra{
		Platform: app.NewPlatformConfig(cfg),
		DB:       scoped,
		// Transaction manager and the process-wide audit recorder. Tier 1
		// services write through the transaction; Tier 2 routes use it for
		// intent rows.
		Tx:           database.NewTxManagerPools(pools, database.WithResolver(app.RLSResolver)),
		Cache:        appCache,
		Audit:        audit.NewPostgresRecorder(scoped),
		Queue:        jobQueue,
		PayloadCodec: rt.payloadCodec,
		Email:        jobs.NewQueuedSender(jobQueue, rt.payloadCodec),
		Storage:      store,
		Events:       rt.events,
		Flags:        flagEvaluator,
		Catalogs:     catalogs,
	}
	return nil
}

// connectDatabase opens the normal pool and, when configured, the dedicated
// bypass pool (both instrumented when tracing asks for it) and records them in
// rt.infra so start closes them if a later step fails.
func (rt *runtime) connectDatabase(ctx context.Context) (database.Pools, error) {
	poolOpts := make([]database.PoolOption, 0, 1)
	if rt.cfg.Tracing.Enabled && rt.cfg.Tracing.InstrumentDB {
		poolOpts = append(poolOpts, database.WithTracerProvider(rt.trace.TracerProvider()))
	}
	pools, err := database.OpenPools(ctx, rt.cfg.DB, poolOpts...)
	if err != nil {
		return database.Pools{}, startupFailed("failed to connect to database", err)
	}
	rt.pools = pools
	slog.Info("connected to database", slog.Bool("bypass_role", pools.Bypass != nil))

	// Schema management is owned by cmd/migrate only. The API never applies
	// DDL, so replicas can start safely without a migration lock convoy.
	slog.Info("database schema assumed up to date (run cmd/migrate)")
	return pools, nil
}

// buildDurableWork creates the job queue, the worker, the event bus, and the
// optional schedule registry. The queue is shared by email, events, and the
// scheduler. Every role registers job handlers on the worker, but only a role
// that runs it (worker, all) claims jobs, limited to JOBS_WORKER_KINDS minus
// JOBS_WORKER_EXCLUDE_KINDS when set. The schedule registry exists only where
// the scheduler can run.
func (rt *runtime) buildDurableWork(pool *pgxpool.Pool, db database.DBTX) *jobs.Queue {
	jobQueue := jobs.NewQueue(db)
	rt.instanceID = newInstanceID()
	workerOpts := []jobs.Option{
		jobs.WithConcurrency(workerConcurrency),
		jobs.WithKinds(rt.cfg.Jobs.WorkerKindList()...),
		jobs.WithExcludeKinds(rt.cfg.Jobs.WorkerExcludeKindList()...),
	}
	if rt.trace.Enabled() {
		workerOpts = append(workerOpts, jobs.WithTracerProvider(rt.trace.TracerProvider()))
	}
	rt.worker = jobs.NewWorker(db, rt.instanceID, workerOpts...)
	rt.events = events.New(jobQueue)
	if rt.cfg.Scheduler.Enabled && rt.cfg.App.RunsWorker() {
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
	modules, err := bootstrap.Build(rt.cfg, rt.infra)
	if err != nil {
		return startupFailed("failed to build modules", err)
	}
	rt.modules = modules

	// Module catalogs (permissions, error codes, flags) are added to the
	// injected instances before anything validates against them: FEATURE_FLAGS
	// keys here, the i18n bundle below, and API-key scopes/permission checks
	// per request.
	if err := rt.infra.Catalogs.Register(modules.List...); err != nil {
		return startupFailed("failed to register module catalogs", err)
	}
	if err := rt.cfg.FeatureFlags.ValidateKnownKeys(rt.infra.Catalogs.Features); err != nil {
		return startupFailed("invalid feature flag configuration", err)
	}
	slog.Info("feature flags initialized", slog.Int("catalog_size", len(rt.infra.Catalogs.Features.Definitions())))

	// Platform and module catalogs are merged and validated at startup so
	// catalog drift (missing vi keys, placeholder mismatch, key collisions)
	// or an unsupported I18N_DEFAULT_LOCALE fails fast instead of serving
	// raw keys.
	rt.i18n, err = i18n.New(rt.cfg.I18N.Locale(), app.LocaleSources(modules.List...)...)
	if err != nil {
		return startupFailed("failed to initialize i18n", err)
	}

	// Module startup checks (tenant existence, permission catalog sync) share
	// the startup deadline. Account creation is not part of startup: the admin
	// comes from the one-shot cmd/seed Job.
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
		Health:         health.NewChecker(rt.pools.Primary, rt.infra.Cache),
		Audit:          rt.infra.Audit,
		TenantResolver: rt.modules.TenantResolver,
		Tracing:        rt.trace,
		I18n:           rt.i18n,
		Flags:          rt.infra.Flags,
		Events:         rt.events,
		Jobs:           rt.worker,
		Schedules:      rt.schedules,
		APIKeyVerifier: rt.modules.APIKeyVerifier,
		Privacy:        rt.modules.Privacy,
		Catalogs:       rt.infra.Catalogs,
	})
	// Module providers register routes, event subscriptions, job handlers
	// (for example webhook delivery/dispatch), and schedules. The bus turns
	// durable subscriptions into worker handlers once every module has
	// registered. APP_ROLE=worker mounts no /api route: the listener serves
	// only health and metrics (readiness checks the database and cache).
	if rt.cfg.App.ServesAPI() {
		rt.app.Register(rt.modules.List...)
	} else {
		rt.app.RegisterBackground(rt.modules.List...)
	}

	// Durable email worker. email.send.encrypted is a platform outbox kind:
	// identity and notifications both enqueue it through jobs.QueuedSender and
	// the sender/codec are platform infrastructure, so no module owns the
	// handler. It shares the pool but runs outside the request path, so SMTP
	// outages retry instead of failing a registration request.
	emailHandler := jobs.EmailHandler(rt.emailSender, rt.payloadCodec)
	rt.worker.Register(jobs.EncryptedEmailJobKind, emailHandler)
}

// startBackground starts the optional scheduler, the job worker, and module
// background loops (webhook retention), which share the worker lifecycle.
//
// APP_ROLE=api starts none of them: jobs it enqueues are processed by worker
// (or all) replicas.
func (rt *runtime) startBackground() (background, error) {
	var bg background
	if !rt.cfg.App.RunsWorker() {
		slog.Info("APP_ROLE=api: job worker, scheduler, and background loops are not started")
		return bg, nil
	}
	if rt.schedules != nil {
		cancel, done, err := rt.startScheduler()
		if err != nil {
			return background{}, err
		}
		bg.schedulerCancel, bg.schedulerDone = cancel, done
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		runGuarded("job worker", func() {
			if err := rt.worker.Run(workerCtx); err != nil {
				slog.Error("job worker stopped", slog.Any("error", err))
			}
		})
		close(workerDone)
	}()
	// Module background loops are best-effort, but they hold the pools and
	// must be joined before closeResources releases them.
	bg.loopsDone = startModuleLoops(workerCtx, rt.modules.Background)
	bg.workerCancel, bg.workerDone = workerCancel, workerDone
	return bg, nil
}

// startModuleLoops runs the module background loops under ctx and returns a
// channel that is closed once every loop has returned. The owner waits for it
// in the drain window: a loop still using the database when the pools close
// would fail every write it attempted during shutdown.
func startModuleLoops(ctx context.Context, loops []func(context.Context)) <-chan struct{} {
	var running sync.WaitGroup
	running.Add(len(loops))
	for _, run := range loops {
		go func() {
			defer running.Done()
			runGuarded("module background loop", func() { run(ctx) })
		}()
	}
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()
	return done
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
	sched := scheduler.New(rt.pools.Primary, rt.infra.Tx, rt.schedules, rt.infra.Queue, rt.instanceID,
		scheduler.WithLeaseTTL(cfg.LeaseTTL),
		scheduler.WithHeartbeatInterval(cfg.HeartbeatInterval),
		scheduler.WithPollInterval(cfg.PollInterval),
		scheduler.WithCatchup(catchup),
		scheduler.WithCatchupLimit(cfg.CatchupLimit),
		scheduler.WithRetention(cfg.Retention),
	)
	schedulerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runGuarded("scheduler", func() {
			if err := sched.Run(schedulerCtx); err != nil {
				slog.Error("scheduler stopped", slog.Any("error", err))
			}
		})
	}()
	slog.Info("scheduler enabled", slog.Int("schedules", rt.schedules.Len()))
	return cancel, done, nil
}

// runGuarded isolates a panic in a background loop. HTTP requests and per-job
// handlers already recover; a bare goroutine would otherwise crash the process
// without a 500 response and without a stack trace.
func runGuarded(name string, run func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("background task panicked",
				slog.String("task", name),
				slog.Any("panic", recovered),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()
	run()
}

// serve listens until the listener fails or SIGINT/SIGTERM arrives. It
// returns exit code 1 for a listener error and 0 for a signal.
func (rt *runtime) serve(ctx context.Context) int {
	serverErr := make(chan error, 1)
	go func() {
		addr := fmt.Sprintf(":%d", rt.cfg.App.Port)
		slog.Info("server starting", slog.String("addr", addr), slog.String("env", rt.cfg.App.Env),
			slog.String("role", rt.cfg.App.EffectiveRole()))
		serverErr <- rt.app.Listen(addr)
	}()

	select {
	case listenErr := <-serverErr:
		if listenErr != nil {
			slog.Error("server error", slog.Any("error", listenErr))
			return 1
		}
	case <-ctx.Done():
		slog.Info("received shutdown signal, shutting down gracefully")
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
	rt.flushMetrics(shutdownDeadline)
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

	// 2. Signal every background producer/consumer before waiting for any one
	//    of them. Waiting on the scheduler before canceling the worker lets a
	//    slow scheduler pass consume the whole drain window while the worker
	//    keeps claiming new jobs. The loops share the worker context, so they
	//    are canceled here too and joined before the pools close.
	if bg.schedulerCancel != nil {
		bg.schedulerCancel()
	}
	if bg.workerCancel != nil {
		bg.workerCancel()
	}
	waitForBackground(ctx, bg)

	clean := true
	if err := <-httpDone; err != nil {
		slog.Error("server forced to shutdown", slog.Any("error", err))
		clean = false
	}
	return clean
}

// flushTraces exports buffered spans after HTTP, workers, and async work have
// stopped. The flush gets TRACING_SHUTDOWN_TIMEOUT capped by what is left of
// the budget after the close reserve, but never less than traceFlushMinimum:
// a drain that used its whole window must not leave tracing with a zero
// deadline. Export failures (for example a collector outage) are logged but
// never change the exit code; losing telemetry must not fail a clean shutdown.
func (rt *runtime) flushTraces(shutdownDeadline time.Time) {
	timeout := rt.cfg.Tracing.ShutdownTimeout
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

// flushMetrics exports buffered OTLP measurements after HTTP, workers, and
// async work have stopped. Prometheus scrapes are pull-based, so the same call
// is a fast no-op for the always-on reader. A collector outage never changes
// the exit code.
func (rt *runtime) flushMetrics(shutdownDeadline time.Time) {
	if rt.metrics == nil {
		return
	}
	timeout := min(defaultMetricsShutdownTimeout, time.Until(shutdownDeadline)-closeReserve)
	timeout = max(timeout, traceFlushMinimum)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := rt.metrics.Shutdown(ctx); err != nil {
		slog.Warn("metrics shutdown incomplete; remaining measurements were dropped", slog.Any("error", err))
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
	closePools(rt.pools.Primary, rt.pools.Bypass, max(time.Until(shutdownDeadline), closeMinimum))
}

// closeDatabasePools closes both runtime pools immediately. It is used on
// startup failure, where there is no shutdown budget to preserve.
func (rt *runtime) closeDatabasePools() {
	if rt.pools.Bypass != nil && rt.pools.Bypass != rt.pools.Primary {
		rt.pools.Bypass.Close()
	}
	if rt.pools.Primary != nil {
		rt.pools.Primary.Close()
	}
}

// closePools closes both database pools without blocking past timeout.
// pgxpool.Close waits for every acquired connection to be released, so a
// handler or job that ignored cancellation would otherwise hold the process
// until Kubernetes sends SIGKILL. On timeout the process exits anyway and the
// server drops the remaining connections.
func closePools(primary, bypass *pgxpool.Pool, timeout time.Duration) {
	pools := []*pgxpool.Pool{primary}
	if bypass != nil && bypass != primary {
		pools = append(pools, bypass)
	}
	done := make(chan struct{})
	go func() {
		for _, pool := range pools {
			if pool != nil {
				pool.Close()
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		acquired := 0
		if primary != nil {
			acquired = int(primary.Stat().AcquiredConns())
		}
		slog.Warn("database pools did not close before the shutdown deadline; connections are still checked out",
			slog.Duration("timeout", timeout),
			slog.Int("acquired_conns", acquired),
		)
	}
}

// waitForBackground joins every background component concurrently under the
// shared drain deadline. Components whose cancellation is already covered by
// another signal (module loops share the worker context) still have their own
// completion channel, so each one is joined before resources close.
func waitForBackground(ctx context.Context, bg background) {
	targets := []struct {
		name string
		done <-chan struct{}
	}{
		{"scheduler", bg.schedulerDone},
		{"job worker", bg.workerDone},
		{"module background loops", bg.loopsDone},
	}
	var wg sync.WaitGroup
	for _, target := range targets {
		if target.done == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitForShutdown(ctx, target.done, target.name)
		}()
	}
	wg.Wait()
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
