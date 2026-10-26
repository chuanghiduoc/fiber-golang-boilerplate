package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

const (
	defaultPollInterval      = time.Second
	defaultConcurrency       = 1
	defaultHeartbeatInterval = 30 * time.Second
	defaultLeaseDuration     = 5 * time.Minute
	defaultJobTimeout        = 10 * time.Minute
	defaultRetention         = 30 * 24 * time.Hour
	defaultPurgeInterval     = time.Hour
	bookkeepingTimeout       = 5 * time.Second
	purgeTimeout             = 5 * time.Second
	maxClaimBackoff          = 30 * time.Second
	jobsInstrumentationName  = "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

// Handler processes a claimed job. Returning an error schedules a retry (or
// marks the job dead once attempts are exhausted).
type Handler func(context.Context, json.RawMessage) error

// Registrar accepts job handlers. *Worker implements it, and the narrow
// interface keeps modules and the event bus independent of the worker's
// configuration surface.
type Registrar interface {
	Register(kind string, handler Handler)
}

var _ Registrar = (*Worker)(nil)

// store is the persistence surface used by Worker. It is an interface so the
// worker's retry, panic, and success paths can be unit tested without a
// PostgreSQL server.
type store interface {
	// Claim and Heartbeat set the job's lease deadline to now() + lease, the
	// calling worker's own lease, so workers with different leases coexist.
	// Claim also reports jobs that lease expiry dead-lettered on their final
	// attempt; they are not dispatched.
	Claim(ctx context.Context, workerID string, limit int, lease time.Duration) (claimResult, error)
	Succeed(ctx context.Context, id int64, workerID string, attempts int) error
	Fail(ctx context.Context, id int64, workerID string, attempts int, reason string, retryAt *time.Time) error
	Heartbeat(ctx context.Context, id int64, workerID string, attempts int, lease time.Duration) error
	Release(ctx context.Context, id int64, workerID string, attempts int, reason string) error
	PurgeTerminal(ctx context.Context, olderThan time.Duration) (int64, error)
}

// claimResult is one Claim call: jobs this worker now owns and, separately,
// jobs that lease expiry dead-lettered while reclaiming expired leases.
type claimResult struct {
	Jobs      []claimedJob
	LeaseDead []leaseExpiredJob
}

// leaseExpiredJob identifies a job dead-lettered by the claim query because
// its worker's lease expired on the job's final attempt.
type leaseExpiredJob struct {
	ID       int64
	TenantID int64
	Kind     string
}

type claimedJob struct {
	ID          int64
	TenantID    int64
	Kind        string
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
	TraceParent string
	// QueueLag is how long the job waited between becoming due (run_at) and
	// this claim, measured by the database clock.
	QueueLag time.Duration
}

// Option configures a Worker.
type Option func(*Worker)

// WithPollInterval sets how long the worker sleeps when no job is due. Values
// less than or equal to zero are ignored.
func WithPollInterval(interval time.Duration) Option {
	return func(w *Worker) {
		if interval > 0 {
			w.pollInterval = interval
		}
	}
}

// WithConcurrency sets the maximum number of handlers that may run at once.
// Values less than one are ignored.
func WithConcurrency(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.concurrency = n
		}
	}
}

// WithHeartbeatInterval sets how often a running handler renews its job lease.
// Values less than or equal to zero are ignored. After all options are applied
// the interval is reduced, if necessary, to stay below the lease duration.
func WithHeartbeatInterval(interval time.Duration) Option {
	return func(w *Worker) {
		if interval > 0 {
			w.heartbeatInterval = interval
		}
	}
}

// WithLeaseDuration sets how long a running job may go without a heartbeat
// before another worker may reclaim it. Values less than or equal to zero are
// ignored. It should be several times the heartbeat interval; otherwise the
// heartbeat interval is reduced to keep a safe margin.
func WithLeaseDuration(d time.Duration) Option {
	return func(w *Worker) {
		if d > 0 {
			w.leaseDuration = d
		}
	}
}

// WithJobTimeout limits how long a handler may run before its context is
// canceled. A handler that then returns an error records a normal failure, so
// the job retries or is dead-lettered by the usual attempt budget; one that
// ignores the cancellation and still returns nil is recorded as succeeded (its
// side effects already happened) and the overrun is logged. Values less than
// or equal to zero disable the timeout.
func WithJobTimeout(d time.Duration) Option {
	return func(w *Worker) {
		if d > 0 {
			w.jobTimeout = d
			return
		}
		w.jobTimeout = 0
	}
}

// WithRetention sets how long terminal (succeeded or dead) job rows are kept.
// The default is 30 days. Values less than or equal to zero disable automatic
// retention, so terminal rows are never purged by the worker.
func WithRetention(d time.Duration) Option {
	return func(w *Worker) {
		if d <= 0 {
			w.retention = 0
			return
		}
		w.retention = d
	}
}

// WithPurgeInterval sets how often the worker runs terminal-job retention.
// The default is one hour. Values less than or equal to zero are ignored.
func WithPurgeInterval(d time.Duration) Option {
	return func(w *Worker) {
		if d > 0 {
			w.purgeInterval = d
		}
	}
}

// WithKinds restricts the worker to claiming only the listed kinds. Use it with
// WithExcludeKinds on the general worker to run a dedicated worker (separate
// pool, concurrency, or deployment) for slow kinds so they cannot
// head-of-line block fast ones. Blank kinds are ignored; no kinds (or only
// blank ones) means no include filter. Register a handler for every listed
// kind: a claimed kind without a handler retries until it is dead-lettered.
func WithKinds(kinds ...string) Option {
	return func(w *Worker) {
		w.includeKinds = normalizeKinds(kinds)
	}
}

// WithExcludeKinds stops the worker from claiming the listed kinds, typically
// the kinds handled by a dedicated WithKinds worker. Blank kinds are ignored.
// When combined with WithKinds both filters apply.
func WithExcludeKinds(kinds ...string) Option {
	return func(w *Worker) {
		w.excludeKinds = normalizeKinds(kinds)
	}
}

// normalizeKinds trims, de-duplicates, and drops blank kinds. It returns nil
// for an empty result so the claim query receives NULL (no filter).
func normalizeKinds(kinds []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		out = append(out, kind)
	}
	return out
}

// WithTracerProvider enables a job.process consumer span per claimed job. Each
// span is a new root with a link to the enqueue-time traceparent, so retries do
// not masquerade as children of the original request. Nil disables tracing.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(w *Worker) {
		if tp == nil {
			return
		}
		w.tracer = tp.Tracer(jobsInstrumentationName)
	}
}

// Worker claims due jobs and dispatches them to registered handlers.
type Worker struct {
	id                string
	pollInterval      time.Duration
	concurrency       int
	retryBase         time.Duration
	retryMax          time.Duration
	heartbeatInterval time.Duration
	leaseDuration     time.Duration
	jobTimeout        time.Duration
	retention         time.Duration
	purgeInterval     time.Duration
	// includeKinds and excludeKinds narrow the claim query (WithKinds,
	// WithExcludeKinds). Nil means no filter.
	includeKinds []string
	excludeKinds []string

	mu       sync.RWMutex
	handlers map[string]Handler

	tracer trace.Tracer

	store store
}

// NewWorker returns a worker that claims jobs through db (a Scoped handle in
// production). workerID is recorded in jobs.locked_by; an empty value becomes
// "worker".
func NewWorker(db database.DBTX, workerID string, opts ...Option) *Worker {
	if strings.TrimSpace(workerID) == "" {
		workerID = "worker"
	}

	w := &Worker{
		id:                workerID,
		pollInterval:      defaultPollInterval,
		concurrency:       defaultConcurrency,
		retryBase:         defaultBackoffBase,
		retryMax:          defaultBackoffMax,
		heartbeatInterval: defaultHeartbeatInterval,
		leaseDuration:     defaultLeaseDuration,
		jobTimeout:        defaultJobTimeout,
		retention:         defaultRetention,
		purgeInterval:     defaultPurgeInterval,
		handlers:          make(map[string]Handler),
		tracer:            noop.NewTracerProvider().Tracer(jobsInstrumentationName),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	w.normalizeLease()
	w.store = &pgStore{db: db, includeKinds: w.includeKinds, excludeKinds: w.excludeKinds}
	return w
}

// normalizeLease enforces a heartbeat interval comfortably below the lease
// duration. The lease is what lets another worker reclaim a job from a crashed
// process, so heartbeats must never race the expiry boundary.
func (w *Worker) normalizeLease() {
	if w.heartbeatInterval <= 0 {
		w.heartbeatInterval = defaultHeartbeatInterval
	}
	if w.leaseDuration <= 0 {
		w.leaseDuration = defaultLeaseDuration
	}
	// Keep at least four heartbeat opportunities per lease. A heartbeat that
	// merely sits below the lease boundary can still race expiry when a query
	// is slow, so clamp to a quarter rather than only when it exceeds the lease.
	if maxInterval := w.leaseDuration / 4; maxInterval > 0 && w.heartbeatInterval > maxInterval {
		w.heartbeatInterval = maxInterval
	}
}

// Register associates kind with handler. Empty kinds and nil handlers are
// ignored, so registration mistakes cannot make the worker panic later.
func (w *Worker) Register(kind string, handler Handler) {
	if w == nil || strings.TrimSpace(kind) == "" || handler == nil {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[kind] = handler
}

// Run claims and processes jobs until ctx is canceled. Cancellation stops new
// claims; handlers already running receive the canceled context and Run waits
// for them before returning nil.
//
// Claim failures are transient by nature (for example a database restart), so
// they are logged and retried with exponential backoff starting at the poll
// interval and capped at 30s. Run therefore returns nil once ctx is canceled,
// never because a claim failed. Bookkeeping failures while finishing a job are
// not fatal either.
func (w *Worker) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}

	slots := make(chan struct{}, w.concurrency)
	finished := make(chan struct{}, w.concurrency)
	claimBackoff := cappedClaimBackoff(w.pollInterval)

	var wg sync.WaitGroup
	defer wg.Wait()

	// Subtract one interval so a fresh worker is immediately due: a process
	// restarted more often than the interval still performs retention.
	lastPurge := time.Now().Add(-w.purgeInterval)

	for ctx.Err() == nil {
		lastPurge = w.purgeIfDue(ctx, lastPurge)

		free := w.concurrency - len(slots)
		if free == 0 {
			// Every slot is busy: wait for a handler to finish (or shutdown).
			select {
			case <-ctx.Done():
			case <-finished:
			}
			continue
		}

		claimed, err := w.store.Claim(ctx, w.id, free, w.leaseDuration)
		if err != nil {
			claimBackoff = w.backOffAfterClaimError(ctx, err, claimBackoff)
			continue
		}
		claimBackoff = cappedClaimBackoff(w.pollInterval)

		for _, dead := range claimed.LeaseDead {
			w.recordLeaseDead(dead)
		}
		if len(claimed.Jobs) == 0 {
			waitFor(ctx, w.pollInterval)
			continue
		}
		for _, job := range claimed.Jobs {
			w.dispatch(ctx, job, slots, finished, &wg)
		}
	}
	return nil
}

// purgeIfDue runs a retention pass when one is due and returns the time of the
// last pass. The time is reset even after a failure so a broken purge cannot
// hammer the database on every claim-loop iteration.
func (w *Worker) purgeIfDue(ctx context.Context, lastPurge time.Time) time.Time {
	if w.retention <= 0 || time.Since(lastPurge) < w.purgeInterval {
		return lastPurge
	}
	w.purgeTerminal(ctx)
	return time.Now()
}

// backOffAfterClaimError records a failed claim, sleeps for the current
// backoff, and returns the next (doubled, capped) backoff. A failure caused by
// shutdown is not an error and returns at once.
func (w *Worker) backOffAfterClaimError(ctx context.Context, err error, backoff time.Duration) time.Duration {
	if ctx.Err() != nil {
		return backoff
	}
	metrics.JobsClaimErrorsTotal.Inc()
	slog.Warn("jobs: claim failed; retrying",
		"worker_id", w.id,
		"retry_in", backoff,
		"error", err,
	)
	waitFor(ctx, backoff)
	return cappedClaimBackoff(backoff * 2)
}

// dispatch runs one claimed job on its own goroutine. Claims never exceed the
// free slot count, so taking a slot does not block and the concurrency limit
// is respected.
func (w *Worker) dispatch(ctx context.Context, job claimedJob, slots, finished chan struct{}, wg *sync.WaitGroup) {
	metrics.JobsClaimedTotal.WithLabelValues(job.Kind).Inc()
	metrics.JobsQueueLagSeconds.WithLabelValues(job.Kind).Observe(job.QueueLag.Seconds())

	slots <- struct{}{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			<-slots
			// finished is only a wake-up hint for the free==0 case. Never
			// block on it: when jobs are sparse the main loop may not be
			// waiting, and a full buffer would leak this goroutine (and hang
			// shutdown in wg.Wait).
			select {
			case finished <- struct{}{}:
			default:
			}
		}()
		if err := w.processJob(ctx, job); err != nil {
			// The lease reclaimer is the safety net; log loudly so a
			// persistent bookkeeping failure is visible in operations.
			slog.Error("jobs: failed to record job outcome",
				"job_id", job.ID,
				"kind", job.Kind,
				"worker_id", w.id,
				"error", err,
			)
		}
	}()
}

// processJob runs one claimed job and records its outcome: success, a release
// when the failure coincided with worker shutdown, or a failure that retries
// or dead-letters by the attempt budget. A handler that hits its own job
// timeout is a real failure, not a shutdown, so it consumes the attempt.
//
// Every store write carries job.Attempts as a fence: if the lease expired and
// another worker reclaimed the job, attempts has advanced and the stale owner's
// write affects no row, returning ErrLeaseLost.
func (w *Worker) processJob(ctx context.Context, job claimedJob) error {
	processCtx, span := w.startJobSpan(ctx, job)
	defer span.End()

	interrupted, leaseLost, handlerErr := w.runHandler(ctx, processCtx, job)
	if leaseLost {
		// The job may already belong to another worker. Its fenced outcome
		// write would affect no row, so there is nothing to record: report the
		// loss instead of counting the canceled handler as a failure.
		return w.recordLeaseLost(span, job)
	}

	// The job has already been claimed in the database, so bookkeeping must
	// still run when the caller's context was canceled during shutdown.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(processCtx), bookkeepingTimeout)
	defer cancel()

	switch {
	case handlerErr == nil:
		return w.recordSuccess(writeCtx, span, job)
	case interrupted:
		return w.recordRelease(writeCtx, span, job, handlerErr)
	default:
		return w.recordFailure(writeCtx, span, job, handlerErr)
	}
}

// runHandler executes the job's handler. The handler is isolated behind
// callHandler so a panic is treated as a normal failure, runs under the
// owning tenant and a child context bounded by the job timeout, and is covered
// by a background heartbeat on the worker context that is stopped and joined
// before returning, so no heartbeat can race the outcome write. interrupted
// reports a failure while the worker itself was stopping, which is most likely
// the shutdown cancellation rather than a verdict on the job. leaseLost reports
// that the heartbeat canceled the handler because the worker no longer owns
// the lease.
func (w *Worker) runHandler(workerCtx, processCtx context.Context, job claimedJob) (interrupted, leaseLost bool, err error) {
	handler, ok := w.handler(job.Kind)
	if !ok {
		return false, false, fmt.Errorf("%w for kind %q", ErrNoHandler, job.Kind)
	}

	// Restore the tenant that owns the job so handlers (email, audit, and
	// future work) see the same context they would inside a request. Rows
	// written before multi-tenancy, and test doubles, may omit the column;
	// those jobs run under the compatibility tenant.
	handlerCtx := tenant.WithID(processCtx, jobTenantID(job))
	timeoutCancel := context.CancelFunc(func() {})
	if w.jobTimeout > 0 {
		handlerCtx, timeoutCancel = context.WithTimeout(handlerCtx, w.jobTimeout)
	}
	// The heartbeat needs a cancelable context with a cause of its own, so a
	// lease loss can be told apart from a job timeout or worker shutdown. A
	// lost lease cancels the handler exactly like any other deadline: the
	// handler must stop doing work now that another worker may own the job.
	handlerCtx, cancelCause := context.WithCancelCause(handlerCtx)
	defer timeoutCancel()
	defer cancelCause(nil)

	stopHeartbeat := w.startHeartbeat(processCtx, cancelCause, job)
	err = callHandler(handlerCtx, handler, job.Payload)
	// Capture both classifications immediately: stopHeartbeat can take time.
	// A disabled timeout must never classify a parent cancellation as a job
	// timeout, and an already-expired parent deadline is not a job timeout.
	timedOut := w.jobTimeout > 0 && workerCtx.Err() == nil && errors.Is(handlerCtx.Err(), context.DeadlineExceeded)
	interrupted = err != nil && workerCtx.Err() != nil
	stopHeartbeat()
	// stopHeartbeat joined the heartbeat goroutine, so no later cancellation
	// can change the cause.
	leaseLost = errors.Is(context.Cause(handlerCtx), ErrLeaseLost)

	if err == nil && timedOut {
		// The handler ignored cancellation and returned after its deadline,
		// but it reported success: its side effects already happened, so
		// recording a failure would re-run them. Record success and make the
		// overrun visible instead.
		slog.Warn("jobs: handler succeeded after exceeding the job timeout",
			"job_id", job.ID,
			"kind", job.Kind,
			"worker_id", w.id,
			"timeout", w.jobTimeout.String(),
		)
	}
	return interrupted, leaseLost, err
}

func (w *Worker) recordSuccess(ctx context.Context, span trace.Span, job claimedJob) error {
	if err := w.store.Succeed(ctx, job.ID, w.id, job.Attempts); err != nil {
		tracing.MarkError(span, err)
		return fmt.Errorf("jobs: mark job %d succeeded: %w", job.ID, err)
	}
	span.SetAttributes(jobOutcomeAttributes("succeeded", nil)...)
	metrics.JobsSucceededTotal.WithLabelValues(job.Kind).Inc()
	return nil
}

// recordRelease requeues a job interrupted by shutdown without consuming the
// attempt, so a drain on the final attempt cannot dead-letter it. Delivery
// stays at-least-once.
func (w *Worker) recordRelease(ctx context.Context, span trace.Span, job claimedJob, handlerErr error) error {
	reason := "jobs: interrupted by worker shutdown: " + handlerErr.Error()
	if err := w.store.Release(ctx, job.ID, w.id, job.Attempts, reason); err != nil {
		tracing.MarkError(span, err)
		return fmt.Errorf("jobs: release job %d: %w", job.ID, err)
	}
	span.SetAttributes(jobOutcomeAttributes("released", nil)...)
	return nil
}

// recordFailure schedules a retry while attempts remain and dead-letters the
// job otherwise. Every failure retries, including an unregistered kind: a
// rollout may register the missing handler before the budget runs out, and
// the attempt cap still dead-letters genuinely unknown work. The exception is
// a handler error wrapped with Permanent, which dead-letters immediately.
func (w *Worker) recordFailure(ctx context.Context, span trace.Span, job claimedJob, handlerErr error) error {
	var retryAt *time.Time
	if !IsPermanent(handlerErr) && job.MaxAttempts > 0 && job.Attempts < job.MaxAttempts {
		next := time.Now().Add(scaledBackoff(job.Attempts, w.retryBase, w.retryMax))
		retryAt = &next
	}

	if err := w.store.Fail(ctx, job.ID, w.id, job.Attempts, handlerErr.Error(), retryAt); err != nil {
		tracing.MarkError(span, err)
		return fmt.Errorf("jobs: mark job %d failed: %w", job.ID, err)
	}

	outcome := "dead"
	if retryAt != nil {
		outcome = "retried"
		metrics.JobsRetriedTotal.WithLabelValues(job.Kind).Inc()
	} else {
		metrics.JobsDeadTotal.WithLabelValues(job.Kind).Inc()
	}
	// Record only the bounded class of failure. handlerErr.Error() is already
	// persisted in jobs.last_error by the store and must not enter telemetry.
	tracing.MarkError(span, handlerErr)
	span.SetAttributes(jobOutcomeAttributes(outcome, retryAt)...)
	return nil
}

// recordLeaseLost reports a handler canceled because this worker lost the job
// lease. It writes no job outcome on purpose: the job may already belong to
// another worker, whose fenced write is authoritative, so recording a failure
// here would double-count the attempt (and could dead-letter work a reclaimer
// is still running).
func (w *Worker) recordLeaseLost(span trace.Span, job claimedJob) error {
	metrics.JobsLeaseLostTotal.WithLabelValues(job.Kind).Inc()
	slog.Warn("jobs: lease lost; canceled handler without recording an outcome",
		"job_id", job.ID,
		"kind", job.Kind,
		"worker_id", w.id,
	)
	tracing.MarkError(span, ErrLeaseLost)
	span.SetAttributes(jobOutcomeAttributes("lease_lost", nil)...)
	return nil
}

// recordLeaseDead reports a job the claim query dead-lettered because its
// lease expired on the final attempt. The claim query already cleared the
// payload and set last_error, so this only makes the loss visible in metrics
// and logs.
func (w *Worker) recordLeaseDead(job leaseExpiredJob) {
	metrics.JobsLeaseDeadTotal.WithLabelValues(job.Kind).Inc()
	slog.Error("jobs: job dead-lettered after worker lease expired at max attempts",
		"job_id", job.ID,
		"kind", job.Kind,
		"tenant_id", job.TenantID,
		"worker_id", w.id,
	)
}

// jobTenantID returns the tenant that owns job, falling back to the
// compatibility tenant for rows (and test doubles) without one.
func jobTenantID(job claimedJob) int64 {
	if job.TenantID == 0 {
		return tenant.DefaultID
	}
	return job.TenantID
}

// startJobSpan creates the per-attempt consumer span. It is always a new root:
// a retry is not a child of the previous attempt. When the enqueue context
// carried a W3C traceparent, a link preserves causality without forcing the
// request's sampling or parentage. A no-op tracer is used when tracing is
// disabled, so the returned span is safe to annotate.
func (w *Worker) startJobSpan(ctx context.Context, job claimedJob) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithNewRoot(),
		trace.WithAttributes(
			attribute.Int64("job.id", job.ID),
			attribute.String("job.kind", job.Kind),
			attribute.Int64("job.tenant_id", jobTenantID(job)),
			attribute.Int("job.attempt", job.Attempts),
			attribute.Int("job.max_attempts", job.MaxAttempts),
		),
	}
	if job.TraceParent != "" {
		remote := trace.SpanContextFromContext(tracing.ExtractRemoteContext(context.Background(), job.TraceParent))
		if remote.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: remote}))
		}
	}
	return w.tracer.Start(ctx, "job.process", opts...)
}

func jobOutcomeAttributes(outcome string, retryAt *time.Time) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("job.outcome", outcome),
		attribute.Bool("job.dead_letter", outcome == "dead"),
	}
	if retryAt != nil {
		attrs = append(attrs, attribute.String("job.retry_at", retryAt.UTC().Format(time.RFC3339)))
	}
	return attrs
}

// purgeTerminal runs one retention pass. It uses a detached context bounded by
// purgeTimeout so a slow DELETE cannot hold shutdown open indefinitely or be
// aborted mid-flight when the worker context is canceled. Every successfully
// deleted row counts toward JobsPurgedTotal, including a partial total returned
// with an error; failures are logged and retried on the next interval.
func (w *Worker) purgeTerminal(ctx context.Context) {
	if w == nil || w.retention <= 0 || w.store == nil {
		return
	}

	purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeTimeout)
	defer cancel()

	purged, err := w.store.PurgeTerminal(purgeCtx, w.retention)
	if purged > 0 {
		metrics.JobsPurgedTotal.Add(float64(purged))
	}
	if err != nil {
		slog.Warn("jobs: purge terminal jobs failed",
			"worker_id", w.id,
			"retention", w.retention,
			"purged", purged,
			"error", err,
		)
	}
}

// startHeartbeat renews the job's lease until the returned stop function is
// called. Heartbeats use a context detached from cancellation so a handler
// that is still unwinding during shutdown keeps ownership until it finishes.
// stop waits for the goroutine, which guarantees no heartbeat runs after the
// worker writes the final outcome. job.Attempts is the worker's claim fence: a
// job reclaimed at a higher attempt no longer matches and reports ErrLeaseLost.
//
// The heartbeat also owns lease-loss cancellation. A fenced heartbeat that
// affects no row reports ErrLeaseLost, so the handler is canceled at once
// instead of continuing to produce side effects another worker will duplicate.
// A transient heartbeat failure is tolerated until the lease duration has
// elapsed since the last successful heartbeat (or the claim), because only then
// may another worker reclaim the job.
func (w *Worker) startHeartbeat(ctx context.Context, cancel context.CancelCauseFunc, job claimedJob) func() {
	if w.store == nil || w.heartbeatInterval <= 0 {
		return func() {}
	}

	hbCtx, hbCancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)

		ticker := time.NewTicker(w.heartbeatInterval)
		defer ticker.Stop()

		// The claim established the lease; treat it as the last success until
		// a heartbeat proves the lease was renewed.
		lastRenewed := time.Now()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				timeout := w.heartbeatInterval
				if timeout > bookkeepingTimeout {
					timeout = bookkeepingTimeout
				}
				callCtx, callCancel := context.WithTimeout(hbCtx, timeout)
				err := w.store.Heartbeat(callCtx, job.ID, w.id, job.Attempts, w.leaseDuration)
				callCancel()
				if err == nil {
					lastRenewed = time.Now()
					continue
				}
				if errors.Is(err, ErrLeaseLost) {
					// Another worker owns the job now. A final outcome write
					// with this attempt fence cannot match, so stop the handler
					// before it duplicates the new owner's work.
					cancel(fmt.Errorf("%w: job %d kind %q: %w", ErrLeaseLost, job.ID, job.Kind, err))
					return
				}
				slog.Warn("jobs: heartbeat failed",
					"job_id", job.ID,
					"kind", job.Kind,
					"worker_id", w.id,
					"error", err,
				)
				if time.Since(lastRenewed) >= w.leaseDuration {
					// The lease can no longer be proven alive: another worker
					// may reclaim the job from now on. Cancel instead of
					// running on with a handler whose outcome can be fenced out.
					cancel(fmt.Errorf("%w: job %d kind %q: heartbeat deadline exceeded", ErrLeaseLost, job.ID, job.Kind))
					return
				}
			}
		}
	}()

	return func() {
		hbCancel()
		<-done
	}
}

func (w *Worker) handler(kind string) (Handler, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	handler, ok := w.handlers[kind]
	return handler, ok
}

// callHandler invokes handler and converts a panic into an error so one bad
// job cannot take down the worker. The stack trace is kept in last_error to
// make the panic diagnosable later.
func callHandler(ctx context.Context, handler Handler, payload json.RawMessage) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("jobs: handler panic: %v\n%s", recovered, debug.Stack())
		}
	}()
	return handler(ctx, payload)
}

func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// cappedClaimBackoff bounds a claim retry delay to the 30s ceiling. A
// non-positive duration falls back to the default poll interval so the worker
// never spins on a misconfigured zero delay.
func cappedClaimBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultPollInterval
	}
	if d > maxClaimBackoff {
		return maxClaimBackoff
	}
	return d
}
