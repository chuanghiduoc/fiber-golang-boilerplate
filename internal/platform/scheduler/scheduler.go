package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

const (
	defaultLeaseTTL          = 30 * time.Second
	defaultHeartbeatInterval = 10 * time.Second
	defaultPollInterval      = 15 * time.Second
	defaultCatchupLimit      = 100
	defaultRetention         = 30 * 24 * time.Hour
	defaultPurgeInterval     = time.Hour
	bookkeepingTimeout       = 5 * time.Second
	maxAcquireBackoff        = 30 * time.Second
)

// ErrNotConfigured is returned by Run when the scheduler lacks a store,
// registry, or queue.
var ErrNotConfigured = errors.New("scheduler: store, registry, and queue are required")

// Option configures a Scheduler.
type Option func(*Scheduler)

// WithLeaseTTL sets how long the leader lease remains valid without a
// heartbeat. Values less than or equal to zero are ignored.
func WithLeaseTTL(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.leaseTTL = d
		}
	}
}

// WithHeartbeatInterval sets how often the leader renews its lease. Values less
// than or equal to zero are ignored, and the interval is clamped to a quarter
// of the lease TTL.
func WithHeartbeatInterval(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.heartbeatInterval = d
		}
	}
}

// WithPollInterval sets how often schedules are evaluated. Values less than or
// equal to zero are ignored.
func WithPollInterval(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.pollInterval = d
		}
	}
}

// WithCatchup sets the catch-up policy for slots missed while no leader ran.
func WithCatchup(policy CatchupPolicy) Option {
	return func(s *Scheduler) {
		switch policy {
		case CatchupNone, CatchupLatest, CatchupAll:
			s.catchup = policy
		}
	}
}

// WithCatchupLimit bounds how many missed slots CatchupAll enqueues per pass.
// Values less than one are ignored.
func WithCatchupLimit(limit int) Option {
	return func(s *Scheduler) {
		if limit > 0 {
			s.catchupLimit = limit
		}
	}
}

// WithRetention sets how long scheduler_runs rows are kept. A non-positive
// value disables automatic retention.
func WithRetention(d time.Duration) Option {
	return func(s *Scheduler) {
		if d <= 0 {
			s.retention = 0
			return
		}
		s.retention = d
	}
}

// WithPurgeInterval sets how often run retention executes. Values less than or
// equal to zero are ignored.
func WithPurgeInterval(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.purgeInterval = d
		}
	}
}

// WithClock overrides the scheduler clock. It is intended for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// Scheduler evaluates the registry on the elected leader and enqueues one
// durable job per due slot.
type Scheduler struct {
	store    store
	registry *Registry
	queue    Queue
	holder   string

	leaseTTL          time.Duration
	heartbeatInterval time.Duration
	pollInterval      time.Duration
	catchup           CatchupPolicy
	catchupLimit      int
	retention         time.Duration
	purgeInterval     time.Duration
	now               func() time.Time
	// leaderSince distinguishes a fresh leadership term (where CatchupNone
	// drops downtime slots) from slow polling during a continuous term (where
	// dropping every tick would starve a schedule shorter than the poll).
	leaderSince time.Time
}

// New returns a PostgreSQL-backed scheduler. holder identifies the process in
// the lease; an empty value becomes "scheduler". tx scopes the transaction
// that records a run and enqueues its job.
func New(pool *pgxpool.Pool, tx *database.TxManager, registry *Registry, queue Queue, holder string, opts ...Option) *Scheduler {
	var st store
	if pool != nil {
		st = newPGStore(pool, tx)
	}
	return newScheduler(st, registry, queue, holder, opts...)
}

func newScheduler(st store, registry *Registry, queue Queue, holder string, opts ...Option) *Scheduler {
	if strings.TrimSpace(holder) == "" {
		holder = "scheduler"
	}
	s := &Scheduler{
		store:             st,
		registry:          registry,
		queue:             queue,
		holder:            holder,
		leaseTTL:          defaultLeaseTTL,
		heartbeatInterval: defaultHeartbeatInterval,
		pollInterval:      defaultPollInterval,
		catchup:           CatchupLatest,
		catchupLimit:      defaultCatchupLimit,
		retention:         defaultRetention,
		purgeInterval:     defaultPurgeInterval,
		now:               func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	if s.leaseTTL <= 0 {
		s.leaseTTL = defaultLeaseTTL
	}
	if s.heartbeatInterval <= 0 {
		s.heartbeatInterval = defaultHeartbeatInterval
	}
	if maxInterval := s.leaseTTL / 4; maxInterval > 0 && s.heartbeatInterval > maxInterval {
		s.heartbeatInterval = maxInterval
	}
	if s.pollInterval <= 0 {
		s.pollInterval = defaultPollInterval
	}
	if s.catchupLimit < 1 {
		s.catchupLimit = defaultCatchupLimit
	}
	if s.purgeInterval <= 0 {
		s.purgeInterval = defaultPurgeInterval
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s
}

// Run acquires the leader lease and evaluates schedules until ctx is canceled.
// While another process holds the lease this loops quietly at the poll
// interval. A lost lease stops scheduling immediately so two leaders cannot
// overlap; the loop then competes for the lease again. Run returns nil on
// cancellation.
func (s *Scheduler) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	if s == nil || s.store == nil || s.registry == nil || s.queue == nil {
		return ErrNotConfigured
	}

	acquireBackoff := cappedBackoff(s.pollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}

		leader, err := s.store.AcquireLease(ctx, s.holder, s.leaseTTL)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("scheduler: lease acquisition failed; retrying",
				"holder", s.holder,
				"retry_in", acquireBackoff,
				"error", err,
			)
			if !waitFor(ctx, acquireBackoff) {
				return nil
			}
			acquireBackoff = cappedBackoff(acquireBackoff * 2)
			continue
		}
		acquireBackoff = cappedBackoff(s.pollInterval)

		if !leader {
			if !waitFor(ctx, s.pollInterval) {
				return nil
			}
			continue
		}

		metrics.SchedulerLeader.Set(1)
		s.runLeader(ctx)
		metrics.SchedulerLeader.Set(0)

		// Release even after a lost lease: the holder predicate prevents this
		// process from releasing another leader's lease.
		s.release(ctx)
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (s *Scheduler) runLeader(ctx context.Context) {
	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	lost := make(chan struct{}, 1)
	stopHeartbeat := s.startHeartbeat(leaderCtx, lost, cancel)
	defer stopHeartbeat()

	lastPurge := time.Time{}
	s.leaderSince = s.now()
	s.runOnce(leaderCtx)
	if s.retention > 0 {
		s.purgeRuns(leaderCtx)
		lastPurge = s.now()
	}

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-lost:
			return
		case <-ticker.C:
			if s.retention > 0 && s.now().Sub(lastPurge) >= s.purgeInterval {
				s.purgeRuns(leaderCtx)
				lastPurge = s.now()
			}
			s.runOnce(leaderCtx)
		}
	}
}

// startHeartbeat renews the lease until ctx ends or the lease is lost. A
// transient renewal error is tolerated while the previous renewal is still
// comfortably inside the lease TTL; stepping down immediately would let a
// brief database or network hiccup leave the cluster without a scheduler. A
// definitive lost lease, or errors lasting until the safety margin expires,
// still cancel the leader before another process can acquire the lease.
func (s *Scheduler) startHeartbeat(ctx context.Context, lost chan<- struct{}, cancelLeader context.CancelFunc) func() {
	hbCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()

		// The successful acquisition established the lease; treat it as the
		// last confirmed renewal until a heartbeat proves otherwise.
		lastRenewed := s.now()
		margin := s.leaseCancelMargin()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				timeout := min(s.heartbeatInterval, bookkeepingTimeout)
				// Measure from before the RPC: a slow or failed call must not
				// make the remaining lease look newer than it is.
				sentAt := s.now()
				callCtx, callCancel := context.WithTimeout(hbCtx, timeout)
				ok, err := s.store.AcquireLease(callCtx, s.holder, s.leaseTTL)
				callCancel()
				if err != nil {
					slog.Warn("scheduler: lease heartbeat failed", "holder", s.holder, "error", err)
					if s.now().Sub(lastRenewed) < margin {
						continue
					}
					slog.Error("scheduler: lease heartbeat deadline exceeded; stepping down", "holder", s.holder)
					// Cancel the leader context so an in-flight runOnce stops
					// enqueueing immediately instead of finishing its pass
					// without a confirmed lease.
					cancelLeader()
					signalLost(lost)
					return
				}
				if !ok {
					slog.Warn("scheduler: lease lost to another holder; stepping down", "holder", s.holder)
					cancelLeader()
					signalLost(lost)
					return
				}
				lastRenewed = sentAt
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// leaseCancelMargin leaves one heartbeat interval for the next renewal and one
// bounded RPC for it to complete before the lease can expire. It is deliberately
// measured from the last successful renewal, not from the first failure.
func (s *Scheduler) leaseCancelMargin() time.Duration {
	callTimeout := min(s.heartbeatInterval, bookkeepingTimeout)
	margin := s.leaseTTL - s.heartbeatInterval - callTimeout
	if margin <= 0 {
		return s.heartbeatInterval
	}
	return margin
}

func (s *Scheduler) release(ctx context.Context) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	if err := s.store.ReleaseLease(releaseCtx, s.holder); err != nil {
		slog.Warn("scheduler: release lease failed", "holder", s.holder, "error", err)
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	now := s.now().UTC()
	for _, schedule := range s.registry.Schedules() {
		if ctx.Err() != nil {
			return
		}
		latest, ok := schedule.latestDue(now)
		if !ok {
			continue
		}

		last, exists, err := s.store.LastRun(ctx, schedule.Name())
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("scheduler: read last run failed",
					"schedule", schedule.Name(),
					"error", err,
				)
			}
			continue
		}

		if !exists {
			s.startSchedule(ctx, schedule, latest)
			continue
		}
		if !latest.After(last) {
			continue
		}

		switch s.catchup {
		case CatchupNone:
			next, ok := schedule.next(last)
			if !ok || next.After(latest) {
				continue
			}
			switch {
			case next.Equal(latest):
				s.enqueue(ctx, schedule, latest)
			case !s.leaderSince.IsZero() && !last.Before(s.leaderSince):
				// Continuous leadership with a poll slower than the schedule:
				// skipping would starve the schedule forever. Run the latest
				// slot and leave older ones behind, which is still "no
				// catch-up".
				s.enqueue(ctx, schedule, latest)
			default:
				s.skip(ctx, schedule, latest, "catch-up disabled")
			}
		case CatchupAll:
			s.catchUp(ctx, schedule, last, latest)
		default:
			s.enqueue(ctx, schedule, latest)
		}
	}
}

// startSchedule handles a schedule with no ledger history. None records the
// current slot as skipped so the first execution lands on the next boundary;
// latest/all execute the current slot once. Without a history anchor there is
// nothing older to replay.
func (s *Scheduler) startSchedule(ctx context.Context, schedule Schedule, latest time.Time) {
	if s.catchup == CatchupNone {
		s.skip(ctx, schedule, latest, "no history; waiting for the next slot")
		return
	}
	s.enqueue(ctx, schedule, latest)
}

func (s *Scheduler) catchUp(ctx context.Context, schedule Schedule, last, latest time.Time) {
	for count := 0; count < s.catchupLimit; count++ {
		next, ok := schedule.next(last)
		if !ok || next.After(latest) {
			return
		}
		if !s.enqueue(ctx, schedule, next) {
			return
		}
		last = next
	}
	slog.Warn("scheduler: catch-up limit reached; remaining slots will replay on later polls",
		"schedule", schedule.Name(),
		"limit", s.catchupLimit,
		"last_run", last,
		"latest_due", latest,
	)
}

func (s *Scheduler) enqueue(ctx context.Context, schedule Schedule, slot time.Time) bool {
	spec := schedule.Spec()
	payload, err := schedule.jobPayload(slot)
	if err != nil {
		slog.Error("scheduler: build job payload failed", "schedule", schedule.Name(), "error", err)
		return false
	}

	run := runRecord{
		Schedule:     schedule.Name(),
		ScheduledFor: slot.UTC(),
		Status:       runStatusEnqueued,
	}
	tenantID := spec.TenantID
	if tenantID == uuid.Nil {
		// Scheduler work runs for the platform, not for a request tenant: an
		// unset spec tenant belongs to the seeded default tenant (a real row),
		// never to whatever tenant happened to be in the leader's context.
		tenantID = tenant.DefaultID
	}
	job := jobs.Job{
		TenantID:       tenantID,
		Kind:           spec.Kind,
		Payload:        payload,
		MaxAttempts:    spec.MaxAttempts,
		IdempotencyKey: schedule.idempotencyKey(slot),
	}

	created, err := s.store.EnqueueRun(ctx, s.queue, run, job)
	if err != nil {
		if ctx.Err() == nil {
			metrics.SchedulerEnqueueErrorsTotal.WithLabelValues(schedule.Name()).Inc()
			slog.Warn("scheduler: enqueue failed",
				"schedule", schedule.Name(),
				"slot", slot,
				"error", err,
			)
		}
		return false
	}
	if created {
		metrics.SchedulerRunsTotal.WithLabelValues(schedule.Name(), "enqueued").Inc()
	} else {
		metrics.SchedulerRunsTotal.WithLabelValues(schedule.Name(), "duplicate").Inc()
	}
	return true
}

func (s *Scheduler) skip(ctx context.Context, schedule Schedule, slot time.Time, reason string) {
	created, err := s.store.SkipRun(ctx, runRecord{
		Schedule:     schedule.Name(),
		ScheduledFor: slot.UTC(),
		Status:       runStatusSkipped,
		Error:        reason,
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("scheduler: record skipped slot failed",
				"schedule", schedule.Name(),
				"slot", slot,
				"error", err,
			)
		}
		return
	}
	if created {
		metrics.SchedulerRunsTotal.WithLabelValues(schedule.Name(), "skipped").Inc()
	}
}

func (s *Scheduler) purgeRuns(ctx context.Context) {
	if s.retention <= 0 {
		return
	}
	purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()

	purged, err := s.store.PurgeRuns(purgeCtx, s.retention)
	if purged > 0 {
		metrics.SchedulerRunsPurgedTotal.Add(float64(purged))
	}
	if err != nil {
		slog.Warn("scheduler: purge run ledger failed",
			"holder", s.holder,
			"retention", s.retention,
			"purged", purged,
			"error", err,
		)
	}
}

func signalLost(lost chan<- struct{}) {
	select {
	case lost <- struct{}{}:
	default:
	}
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

func cappedBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultPollInterval
	}
	if d > maxAcquireBackoff {
		return maxAcquireBackoff
	}
	return d
}
