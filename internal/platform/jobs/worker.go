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

	"github.com/jackc/pgx/v5/pgxpool"

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
	purgeTimeout             = 10 * time.Second
	maxClaimBackoff          = 30 * time.Second
)

// Handler processes a claimed job. Returning an error schedules a retry (or
// marks the job dead once attempts are exhausted).
type Handler func(context.Context, json.RawMessage) error

// store is the persistence surface used by Worker. It is an interface so the
// worker's retry, panic, and success paths can be unit tested without a
// PostgreSQL server.
type store interface {
	Claim(ctx context.Context, workerID string, limit int, staleAfter time.Duration) ([]claimedJob, error)
	Succeed(ctx context.Context, id int64, workerID string, attempts int) error
	Fail(ctx context.Context, id int64, workerID string, attempts int, reason string, retryAt *time.Time) error
	Heartbeat(ctx context.Context, id int64, workerID string, attempts int) error
	PurgeTerminal(ctx context.Context, olderThan time.Duration) (int64, error)
}

type claimedJob struct {
	ID          int64
	Kind        string
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
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
// canceled. The worker then records a normal failure, so the job retries or is
// dead-lettered by the usual attempt budget. Values less than or equal to zero
// disable the timeout.
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

// Worker claims due jobs and dispatches them to registered handlers.
type Worker struct {
	pool              *pgxpool.Pool
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

	mu       sync.RWMutex
	handlers map[string]Handler

	store store
}

// NewWorker returns a worker that claims jobs through pool. workerID is
// recorded in jobs.locked_by; an empty value becomes "worker".
func NewWorker(pool *pgxpool.Pool, workerID string, opts ...Option) *Worker {
	if strings.TrimSpace(workerID) == "" {
		workerID = "worker"
	}

	w := &Worker{
		pool:              pool,
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
		store:             newPGStore(pool),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	w.normalizeLease()
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

	lastPurge := time.Now()

	for {
		if ctx.Err() != nil {
			return nil
		}

		if w.retention > 0 && time.Since(lastPurge) >= w.purgeInterval {
			w.purgeTerminal(ctx)
			// Reset even after a failure so a broken purge cannot hammer the
			// database on every claim-loop iteration.
			lastPurge = time.Now()
		}

		free := w.concurrency - len(slots)
		if free == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-finished:
			}
			continue
		}

		claimed, err := w.store.Claim(ctx, w.id, free, w.leaseDuration)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			metrics.JobsClaimErrorsTotal.Inc()
			slog.Warn("jobs: claim failed; retrying",
				"worker_id", w.id,
				"retry_in", claimBackoff,
				"error", err,
			)
			if !waitFor(ctx, claimBackoff) {
				return nil
			}
			claimBackoff = cappedClaimBackoff(claimBackoff * 2)
			continue
		}
		claimBackoff = cappedClaimBackoff(w.pollInterval)

		if len(claimed) == 0 {
			if !waitFor(ctx, w.pollInterval) {
				return nil
			}
			continue
		}

		for _, job := range claimed {
			metrics.JobsClaimedTotal.WithLabelValues(job.Kind).Inc()
			// Claims never exceed the free slot count, so this send does not
			// block and the concurrency limit is respected.
			slots <- struct{}{}
			wg.Add(1)
			go func(job claimedJob) {
				defer wg.Done()
				defer func() {
					<-slots
					// finished is only a wake-up hint for the free==0 case.
					// Never block on it: when jobs are sparse the main loop may
					// not be waiting, and a full buffer would leak this goroutine
					// (and hang shutdown in wg.Wait).
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
			}(job)
		}
	}
}

// processJob runs one claimed job and records its outcome. The handler is
// always isolated behind callHandler so a panic is treated as a normal
// failure. While the handler runs a background heartbeat keeps the job's lease
// alive; it uses the worker context and is stopped and joined before the final
// success/failure write so no heartbeat can race that write. The handler gets a
// child context bounded by the job timeout, which does not affect the
// heartbeat.
//
// Every store write carries job.Attempts as a fence: if the lease expired and
// another worker reclaimed the job, attempts has advanced and the stale owner's
// write affects no row, returning ErrLeaseLost.
func (w *Worker) processJob(ctx context.Context, job claimedJob) error {
	handler, ok := w.handler(job.Kind)
	var handlerErr error
	if !ok {
		handlerErr = fmt.Errorf("%w for kind %q", ErrNoHandler, job.Kind)
	} else {
		handlerCtx := ctx
		var cancel context.CancelFunc
		if w.jobTimeout > 0 {
			handlerCtx, cancel = context.WithTimeout(ctx, w.jobTimeout)
		}

		stopHeartbeat := w.startHeartbeat(ctx, job.ID, job.Attempts)
		handlerErr = callHandler(handlerCtx, handler, job.Payload)
		// Capture the deadline result immediately: stopHeartbeat can take time,
		// and a disabled timeout must never classify a parent cancellation as a
		// job timeout. Require the parent context to still be active so an
		// already-expired parent deadline is not mislabeled as a job timeout.
		timedOut := w.jobTimeout > 0 && ctx.Err() == nil && errors.Is(handlerCtx.Err(), context.DeadlineExceeded)
		if cancel != nil {
			cancel()
		}
		stopHeartbeat()
		if handlerErr == nil && timedOut {
			// The handler ignored cancellation and returned after its deadline.
			handlerErr = fmt.Errorf("jobs: handler exceeded the %s job timeout", w.jobTimeout)
		}
	}

	// The job has already been claimed in the database, so bookkeeping must
	// still run when the caller's context was canceled during shutdown.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()

	if handlerErr == nil {
		if err := w.store.Succeed(writeCtx, job.ID, w.id, job.Attempts); err != nil {
			return fmt.Errorf("jobs: mark job %d succeeded: %w", job.ID, err)
		}
		metrics.JobsSucceededTotal.WithLabelValues(job.Kind).Inc()
		return nil
	}

	reason := handlerErr.Error()
	var retryAt *time.Time
	// Every failure retries while attempts remain, including an unregistered
	// kind. A rollout may register the missing handler before the budget runs
	// out, and the attempt cap still dead-letters genuinely unknown work.
	if job.MaxAttempts > 0 && job.Attempts < job.MaxAttempts {
		next := time.Now().Add(scaledBackoff(job.Attempts, w.retryBase, w.retryMax))
		retryAt = &next
	}

	if err := w.store.Fail(writeCtx, job.ID, w.id, job.Attempts, reason, retryAt); err != nil {
		return fmt.Errorf("jobs: mark job %d failed: %w", job.ID, err)
	}
	if retryAt != nil {
		metrics.JobsRetriedTotal.WithLabelValues(job.Kind).Inc()
	} else {
		metrics.JobsDeadTotal.WithLabelValues(job.Kind).Inc()
	}
	return nil
}

// purgeTerminal runs one retention pass. It uses a detached context bounded by
// purgeTimeout so a slow DELETE cannot hold shutdown open indefinitely or be
// aborted mid-flight when the worker context is canceled. Success is counted in
// JobsPurgedTotal; failures are logged and retried on the next interval.
func (w *Worker) purgeTerminal(ctx context.Context) {
	if w == nil || w.retention <= 0 || w.store == nil {
		return
	}

	purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), purgeTimeout)
	defer cancel()

	purged, err := w.store.PurgeTerminal(purgeCtx, w.retention)
	if err != nil {
		slog.Warn("jobs: purge terminal jobs failed",
			"worker_id", w.id,
			"retention", w.retention,
			"error", err,
		)
		return
	}
	metrics.JobsPurgedTotal.Add(float64(purged))
}

// startHeartbeat renews the job's lease until the returned stop function is
// called. Heartbeats use a context detached from cancellation so a handler
// that is still unwinding during shutdown keeps ownership until it finishes.
// stop waits for the goroutine, which guarantees no heartbeat runs after the
// worker writes the final outcome. attempts is the worker's claim fence: a job
// reclaimed at a higher attempt no longer matches and reports ErrLeaseLost.
func (w *Worker) startHeartbeat(ctx context.Context, id int64, attempts int) func() {
	if w.store == nil || w.heartbeatInterval <= 0 {
		return func() {}
	}

	hbCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)

		ticker := time.NewTicker(w.heartbeatInterval)
		defer ticker.Stop()

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
				err := w.store.Heartbeat(callCtx, id, w.id, attempts)
				callCancel()
				if err != nil {
					// A missed heartbeat is not fatal here: either the lease is
					// still valid or the job has already been reclaimed, in
					// which case the final Succeed/Fail write is authoritative.
					slog.Warn("jobs: heartbeat failed",
						"job_id", id,
						"worker_id", w.id,
						"error", err,
					)
				}
			}
		}
	}()

	return func() {
		cancel()
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
