package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

type noopQueue struct{}

func (noopQueue) EnqueueOnceTx(context.Context, pgx.Tx, jobs.Job) (id uuid.UUID, created bool, err error) {
	return testID(1), true, nil
}

type fakeStore struct {
	mu sync.Mutex

	acquireResults []bool
	acquireErrors  []error
	acquireDefault bool
	acquireCalls   int
	releaseCalls   int

	lastRuns    map[string]time.Time
	lastRunErr  error
	ledger      map[string]runRecord
	jobs        []jobs.Job
	skips       []runRecord
	enqueueErr  error
	enqueued    chan jobs.Job
	purgeCount  int64
	purgeErr    error
	purgeWindow time.Duration
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		acquireDefault: true,
		lastRuns:       make(map[string]time.Time),
		ledger:         make(map[string]runRecord),
		enqueued:       make(chan jobs.Job, 16),
	}
}

func (s *fakeStore) AcquireLease(context.Context, string, time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquireCalls++
	if len(s.acquireErrors) > 0 {
		err := s.acquireErrors[0]
		s.acquireErrors = s.acquireErrors[1:]
		return false, err
	}
	if len(s.acquireResults) > 0 {
		result := s.acquireResults[0]
		s.acquireResults = s.acquireResults[1:]
		return result, nil
	}
	return s.acquireDefault, nil
}

func (s *fakeStore) ReleaseLease(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls++
	return nil
}

func (s *fakeStore) LastRun(_ context.Context, schedule string) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRunErr != nil {
		return time.Time{}, false, s.lastRunErr
	}
	slot, ok := s.lastRuns[schedule]
	return slot, ok, nil
}

func (s *fakeStore) EnqueueRun(_ context.Context, _ Queue, run runRecord, job jobs.Job) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueueErr != nil {
		return false, s.enqueueErr
	}
	key := runKey(run.Schedule, run.ScheduledFor)
	if _, exists := s.ledger[key]; exists {
		return false, nil
	}
	s.ledger[key] = run
	s.lastRuns[run.Schedule] = run.ScheduledFor
	s.jobs = append(s.jobs, job)
	select {
	case s.enqueued <- job:
	default:
	}
	return true, nil
}

func (s *fakeStore) SkipRun(_ context.Context, run runRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := runKey(run.Schedule, run.ScheduledFor)
	if _, exists := s.ledger[key]; exists {
		return false, nil
	}
	s.ledger[key] = run
	s.lastRuns[run.Schedule] = run.ScheduledFor
	s.skips = append(s.skips, run)
	return true, nil
}

func (s *fakeStore) PurgeRuns(_ context.Context, olderThan time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeWindow = olderThan
	return s.purgeCount, s.purgeErr
}

func (s *fakeStore) setLast(schedule string, slot time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRuns[schedule] = slot
	s.ledger[runKey(schedule, slot)] = runRecord{Schedule: schedule, ScheduledFor: slot}
}

func (s *fakeStore) enqueuedJobs() []jobs.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]jobs.Job(nil), s.jobs...)
}

func (s *fakeStore) skippedRuns() []runRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runRecord(nil), s.skips...)
}

func runKey(schedule string, slot time.Time) string {
	return schedule + "|" + slot.UTC().Format(time.RFC3339Nano)
}

func fixedClock(now time.Time) Option {
	return WithClock(func() time.Time { return now })
}

func testScheduler(t *testing.T, st *fakeStore, spec Spec, opts ...Option) *Scheduler {
	t.Helper()
	registry := NewRegistry()
	require.NoError(t, registry.Register(spec))
	base := make([]Option, 0, 2+len(opts))
	base = append(base, WithPollInterval(10*time.Millisecond), WithCatchupLimit(100))
	return newScheduler(st, registry, noopQueue{}, "test-scheduler", append(base, opts...)...)
}

func metricCounter(t *testing.T, name string) float64 {
	t.Helper()
	value, _ := metrics.SnapshotValue(name)
	return value
}

func TestSchedulerRunOnceEnqueuesLatestSlot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 7, 30, 0, time.UTC)
	st := newFakeStore()
	s := testScheduler(t, st, Spec{
		Name:     "cleanup",
		Interval: 5 * time.Minute,
		Kind:     "maintenance.cleanup",
		TenantID: testID(4),
		Payload:  json.RawMessage(`{"batch":10}`),
	}, fixedClock(now), WithCatchup(CatchupLatest))

	s.runOnce(context.Background())
	enqueued := st.enqueuedJobs()
	require.Len(t, enqueued, 1)
	assert.Equal(t, "maintenance.cleanup", enqueued[0].Kind)
	assert.Equal(t, testID(4), enqueued[0].TenantID)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:05:00Z", enqueued[0].IdempotencyKey)
	assert.JSONEq(t, `{"schedule":"cleanup","scheduledFor":"2026-10-24T12:05:00Z","data":{"batch":10}}`, string(enqueued[0].Payload))

	// The same slot is not enqueued twice.
	s.runOnce(context.Background())
	assert.Len(t, st.enqueuedJobs(), 1)

	// The next interval becomes due at 12:10.
	s.now = func() time.Time { return now.Add(3 * time.Minute) }
	s.runOnce(context.Background())
	enqueued = st.enqueuedJobs()
	require.Len(t, enqueued, 2)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:10:00Z", enqueued[1].IdempotencyKey)
}

func TestSchedulerCatchupNoneSkipsMissedSlots(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 20, 0, 0, time.UTC)
	st := newFakeStore()
	st.setLast("cleanup", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"},
		fixedClock(now), WithCatchup(CatchupNone))
	s.leaderSince = now

	s.runOnce(context.Background())
	assert.Empty(t, st.enqueuedJobs(), "missed slots must not be enqueued")
	skips := st.skippedRuns()
	require.Len(t, skips, 1)
	assert.Equal(t, time.Date(2026, 10, 24, 12, 20, 0, 0, time.UTC), skips[0].ScheduledFor)
	assert.Equal(t, runStatusSkipped, skips[0].Status)

	// The next boundary runs normally.
	s.now = func() time.Time { return now.Add(5 * time.Minute) }
	s.runOnce(context.Background())
	enqueued := st.enqueuedJobs()
	require.Len(t, enqueued, 1)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:25:00Z", enqueued[0].IdempotencyKey)
}

func TestSchedulerCatchupNoneInitialWaitsForNextBoundary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 7, 0, 0, time.UTC)
	st := newFakeStore()
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"},
		fixedClock(now), WithCatchup(CatchupNone))

	s.runOnce(context.Background())
	assert.Empty(t, st.enqueuedJobs())
	require.Len(t, st.skippedRuns(), 1)

	s.now = func() time.Time { return now.Add(3 * time.Minute) }
	s.runOnce(context.Background())
	require.Len(t, st.enqueuedJobs(), 1)
}

func TestSchedulerCatchupNoneDoesNotStarveShortSchedules(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 20, 0, 0, time.UTC)
	st := newFakeStore()
	st.setLast("cleanup", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"},
		fixedClock(now), WithCatchup(CatchupNone))
	// The missed window happened during continuous leadership (for example a
	// poll that ran late), not while another process was leader.
	s.leaderSince = now.Add(-time.Hour)

	s.runOnce(context.Background())
	enqueued := st.enqueuedJobs()
	require.Len(t, enqueued, 1, "a slow poll must not starve the schedule")
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:20:00Z", enqueued[0].IdempotencyKey)
}

func TestSchedulerCatchupLatestRunsOnlyNewestMissedSlot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 20, 0, 0, time.UTC)
	st := newFakeStore()
	st.setLast("cleanup", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"},
		fixedClock(now), WithCatchup(CatchupLatest))

	s.runOnce(context.Background())
	enqueued := st.enqueuedJobs()
	require.Len(t, enqueued, 1)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:20:00Z", enqueued[0].IdempotencyKey)
}

func TestSchedulerCatchupAllIsBoundedPerPass(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 20, 0, 0, time.UTC)
	st := newFakeStore()
	st.setLast("cleanup", time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"},
		fixedClock(now), WithCatchup(CatchupAll), WithCatchupLimit(2))

	s.runOnce(context.Background())
	enqueued := st.enqueuedJobs()
	require.Len(t, enqueued, 2)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:05:00Z", enqueued[0].IdempotencyKey)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:10:00Z", enqueued[1].IdempotencyKey)

	// The limit is per pass, not permanent: the next poll continues.
	s.runOnce(context.Background())
	enqueued = st.enqueuedJobs()
	require.Len(t, enqueued, 4)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:15:00Z", enqueued[2].IdempotencyKey)
	assert.Equal(t, "scheduler:cleanup:2026-10-24T12:20:00Z", enqueued[3].IdempotencyKey)
}

func TestSchedulerEnqueueErrorDoesNotAdvanceLedger(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 7, 0, 0, time.UTC)
	st := newFakeStore()
	st.enqueueErr = errors.New("database unavailable")
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: 5 * time.Minute, Kind: "maintenance.cleanup"},
		fixedClock(now), WithCatchup(CatchupLatest))

	s.runOnce(context.Background())
	assert.Empty(t, st.enqueuedJobs())

	st.mu.Lock()
	st.enqueueErr = nil
	st.mu.Unlock()
	s.runOnce(context.Background())
	assert.Len(t, st.enqueuedJobs(), 1, "a failed enqueue must be retried")
}

func TestSchedulerRunAcquiresLeaseEnqueuesAndStops(t *testing.T) {
	st := newFakeStore()
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: time.Minute, Kind: "maintenance.cleanup"},
		WithCatchup(CatchupLatest))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	select {
	case <-st.enqueued:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("scheduler did not enqueue a due slot")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	assert.GreaterOrEqual(t, st.acquireCalls, 1)
	assert.GreaterOrEqual(t, st.releaseCalls, 1)
}

func TestSchedulerRunWaitsForLeader(t *testing.T) {
	st := newFakeStore()
	st.acquireDefault = false
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: time.Minute, Kind: "maintenance.cleanup"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.NoError(t, s.Run(ctx))
	assert.Empty(t, st.enqueuedJobs())
	st.mu.Lock()
	defer st.mu.Unlock()
	assert.Equal(t, 0, st.releaseCalls)
}

func TestSchedulerHeartbeatLossStepsDown(t *testing.T) {
	st := newFakeStore()
	st.acquireResults = []bool{false}
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: time.Minute, Kind: "maintenance.cleanup"},
		WithCatchup(CatchupLatest), WithHeartbeatInterval(5*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.runLeader(ctx) // returns on the first failed heartbeat, not the poll interval
	require.Len(t, st.enqueuedJobs(), 1)
}

func TestSchedulerLeaseErrorDoesNotRunSchedules(t *testing.T) {
	st := newFakeStore()
	st.acquireErrors = []error{errors.New("lease unavailable")}
	st.acquireDefault = false
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: time.Minute, Kind: "maintenance.cleanup"})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	require.NoError(t, s.Run(ctx))
	assert.Empty(t, st.enqueuedJobs())
}

func TestSchedulerPurgesRuns(t *testing.T) {
	st := newFakeStore()
	st.purgeCount = 3
	s := testScheduler(t, st, Spec{Name: "cleanup", Interval: time.Minute, Kind: "maintenance.cleanup"},
		WithRetention(24*time.Hour))

	before := metricCounter(t, "scheduler_runs_purged_total")
	s.purgeRuns(context.Background())
	st.mu.Lock()
	window := st.purgeWindow
	st.mu.Unlock()
	assert.Equal(t, 24*time.Hour, window)
	assert.Equal(t, before+3, metricCounter(t, "scheduler_runs_purged_total"))
}

func TestSchedulerOptionsNormalize(t *testing.T) {
	t.Parallel()

	s := newScheduler(newFakeStore(), NewRegistry(), noopQueue{}, "",
		WithLeaseTTL(100*time.Second),
		WithHeartbeatInterval(50*time.Second),
		WithPollInterval(0),
		WithCatchup("bogus"),
		WithCatchupLimit(0),
		WithRetention(-1),
	)
	assert.Equal(t, "scheduler", s.holder)
	assert.Equal(t, 100*time.Second, s.leaseTTL)
	assert.Equal(t, 25*time.Second, s.heartbeatInterval, "heartbeat must stay below the lease")
	assert.Equal(t, defaultPollInterval, s.pollInterval)
	assert.Equal(t, CatchupLatest, s.catchup)
	assert.Equal(t, defaultCatchupLimit, s.catchupLimit)
	assert.Equal(t, time.Duration(0), s.retention)
}

func TestSchedulerRunRejectsIncompleteConfiguration(t *testing.T) {
	s := &Scheduler{}
	require.ErrorIs(t, s.Run(context.Background()), ErrNotConfigured)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, (&Scheduler{}).Run(ctx), "an already-canceled context exits cleanly")
}

// TestSchedulerDefaultsUnsetSpecTenant pins the platform-job tenant: a
// schedule that does not name a tenant enqueues for the seeded default tenant
// (a real row), not for the leader's request context.
func TestSchedulerDefaultsUnsetSpecTenant(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 24, 12, 7, 30, 0, time.UTC)
	st := newFakeStore()
	s := testScheduler(t, st, Spec{
		Name:     "default-tenant",
		Interval: 5 * time.Minute,
		Kind:     "maintenance.cleanup",
	}, fixedClock(now), WithCatchup(CatchupLatest))

	s.runOnce(context.Background())
	enqueued := st.enqueuedJobs()
	require.Len(t, enqueued, 1)
	assert.Equal(t, tenant.DefaultID, enqueued[0].TenantID)
}
