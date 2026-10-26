//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// startStressWorker runs a worker until the returned stop function is called
// and waits for a clean shutdown. It is registered as test cleanup so a failing
// assertion cannot leave workers running against the test schema.
func startStressWorker(t *testing.T, worker *Worker) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				assert.NoError(t, err, "worker %q must stop cleanly", worker.id)
			case <-time.After(5 * time.Second):
				t.Errorf("worker %q did not stop after cancellation", worker.id)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestWorkerStressIntegrationProcessesAllJobsExactlyOnce enqueues 100 jobs and
// processes them with eight workers (some sharing a worker ID) at concurrency
// four. It asserts every job reaches succeeded, is handled exactly once, and
// leaves no queued or running row behind.
func TestWorkerStressIntegrationProcessesAllJobsExactlyOnce(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 25*time.Second)
	defer cancel()

	const (
		total             = 100
		workerCount       = 8
		workerConcurrency = 4
		kind              = "jobs.stress.integration.exactly_once"
		handlerDelay      = 5 * time.Millisecond
	)

	queue := NewQueue(pgtest.Scoped(pool))
	dbIDs := make(map[int]int64, total)
	for i := 0; i < total; i++ {
		id, err := queue.Enqueue(ctx, Job{
			Kind:        kind,
			Payload:     json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i)),
			MaxAttempts: 3,
		})
		require.NoError(t, err)
		dbIDs[i] = id
	}

	var (
		handledMu sync.Mutex
		handled   = make(map[int64]int, total)
	)
	seen := make(chan int64, total)
	handler := func(_ context.Context, payload json.RawMessage) error {
		var item struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		id, ok := dbIDs[item.Seq]
		if !ok {
			return fmt.Errorf("unexpected seq %d", item.Seq)
		}

		handledMu.Lock()
		handled[id]++
		first := handled[id] == 1
		handledMu.Unlock()
		if first {
			seen <- id
		}

		time.Sleep(handlerDelay)
		return nil
	}

	stops := make([]func(), 0, workerCount)
	for i := 0; i < workerCount; i++ {
		workerID := fmt.Sprintf("stress-unique-%d", i)
		if i%2 == 0 {
			// Replicas sharing a configured worker name must not affect the
			// claim or attempt fences.
			workerID = "stress-shared"
		}
		worker := NewWorker(pgtest.Scoped(pool), workerID,
			WithConcurrency(workerConcurrency),
			WithPollInterval(10*time.Millisecond),
			WithRetention(0),
			WithLeaseDuration(30*time.Second),
			WithHeartbeatInterval(5*time.Second),
		)
		worker.Register(kind, handler)
		stops = append(stops, startStressWorker(t, worker))
	}

	for i := 0; i < total; i++ {
		select {
		case <-seen:
		case <-ctx.Done():
			t.Fatalf("handled %d/%d jobs before timeout", i, total)
		}
	}
	for _, stop := range stops {
		stop()
	}

	handledMu.Lock()
	require.Len(t, handled, total)
	for id, count := range handled {
		assert.Equal(t, 1, count, "job %d must be handled exactly once", id)
	}
	handledMu.Unlock()

	rows, err := pool.Query(ctx,
		`SELECT status, count(*) FROM jobs WHERE kind = $1 GROUP BY status`, kind)
	require.NoError(t, err)
	defer rows.Close()

	statuses := make(map[string]int64, 2)
	for rows.Next() {
		var (
			status string
			count  int64
		)
		require.NoError(t, rows.Scan(&status, &count))
		statuses[status] = count
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, int64(total), statuses["succeeded"])
	assert.Zero(t, statuses["queued"], "no job may remain queued")
	assert.Zero(t, statuses["running"], "no job may remain running")
	assert.Zero(t, statuses["dead"])

	var attempts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COALESCE(max(attempts), 0) FROM jobs WHERE kind = $1`, kind).Scan(&attempts))
	assert.Equal(t, 1, attempts, "every job must succeed on its first claim")

	var leased int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = $1 AND (locked_by IS NOT NULL OR locked_at IS NOT NULL)`,
		kind).Scan(&leased))
	assert.Zero(t, leased, "a succeeded job must release its lease")
}

// TestWorkerStressIntegrationRetriesAndDeadLetters mixes deterministic failures
// with successes. Jobs whose payload sequence is in the first three of every ten
// always fail and must dead-letter after five attempts; the rest must succeed on
// the first attempt exactly once.
func TestWorkerStressIntegrationRetriesAndDeadLetters(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 25*time.Second)
	defer cancel()

	const (
		total             = 100
		workerCount       = 4
		workerConcurrency = 4
		maxAttempts       = 5
		kind              = "jobs.stress.integration.retry"
		handlerDelay      = time.Millisecond
	)
	poison := func(seq int) bool { return seq%10 < 3 }
	expectedPoison := total / 10 * 3

	queue := NewQueue(pgtest.Scoped(pool))
	for i := 0; i < total; i++ {
		_, err := queue.Enqueue(ctx, Job{
			Kind:        kind,
			Payload:     json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i)),
			MaxAttempts: maxAttempts,
		})
		require.NoError(t, err)
	}

	var (
		mu        sync.Mutex
		invoked   = make(map[int]int, total)
		succeeded = make(map[int]int, total)
	)
	handler := func(_ context.Context, payload json.RawMessage) error {
		var item struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}

		mu.Lock()
		invoked[item.Seq]++
		mu.Unlock()

		if poison(item.Seq) {
			return errors.New("deterministic poison job")
		}

		time.Sleep(handlerDelay)
		mu.Lock()
		succeeded[item.Seq]++
		mu.Unlock()
		return nil
	}

	stops := make([]func(), 0, workerCount)
	for i := 0; i < workerCount; i++ {
		worker := NewWorker(pgtest.Scoped(pool), fmt.Sprintf("stress-retry-%d", i),
			WithConcurrency(workerConcurrency),
			WithPollInterval(5*time.Millisecond),
			WithRetention(0),
			WithLeaseDuration(30*time.Second),
			WithHeartbeatInterval(5*time.Second),
		)
		worker.retryBase = time.Millisecond
		worker.retryMax = 10 * time.Millisecond
		worker.Register(kind, handler)
		stops = append(stops, startStressWorker(t, worker))
	}

	require.Eventually(t, func() bool {
		var terminal int64
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM jobs
			WHERE kind = $1 AND status IN ('succeeded', 'dead')`, kind).Scan(&terminal)
		return err == nil && terminal == total
	}, 20*time.Second, 20*time.Millisecond,
		"every job must eventually succeed or dead-letter")

	for _, stop := range stops {
		stop()
	}

	var succeededCount, deadCount int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'succeeded'),
		       count(*) FILTER (WHERE status = 'dead')
		FROM jobs WHERE kind = $1`, kind).Scan(&succeededCount, &deadCount))
	assert.Equal(t, int64(total-expectedPoison), succeededCount)
	assert.Equal(t, int64(expectedPoison), deadCount)

	var queuedOrRunning int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE kind = $1 AND status IN ('queued', 'running')`, kind).Scan(&queuedOrRunning))
	assert.Zero(t, queuedOrRunning)

	var succeededAttempts int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(max(attempts), 0) FROM jobs
		WHERE kind = $1 AND status = 'succeeded'`, kind).Scan(&succeededAttempts))
	assert.Equal(t, 1, succeededAttempts, "healthy jobs must succeed on their first attempt")

	var minDeadAttempts, maxDeadAttempts int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COALESCE(min(attempts), 0), COALESCE(max(attempts), 0) FROM jobs
		WHERE kind = $1 AND status = 'dead'`, kind).Scan(&minDeadAttempts, &maxDeadAttempts))
	assert.Equal(t, maxAttempts, minDeadAttempts)
	assert.Equal(t, maxAttempts, maxDeadAttempts)

	var payloadsCleared int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs WHERE kind = $1 AND payload = '{}'::jsonb`, kind).
		Scan(&payloadsCleared))
	assert.Equal(t, int64(total), payloadsCleared,
		"terminal jobs must not retain their payload")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, invoked, total)
	require.Len(t, succeeded, total-expectedPoison)
	for seq, count := range invoked {
		if poison(seq) {
			assert.Equal(t, maxAttempts, count,
				"poison job %d must be invoked once per allowed attempt", seq)
			continue
		}
		assert.Equal(t, 1, count, "healthy job %d must be invoked exactly once", seq)
	}
	for seq, count := range succeeded {
		assert.Equal(t, 1, count, "job %d must be recorded successful exactly once", seq)
	}
}

// TestWorkerStressIntegrationRetentionRace runs PurgeTerminal while workers are
// processing. Old terminal rows must be deleted; queued and running rows,
// including deliberately old ones, must never be touched.
func TestWorkerStressIntegrationRetentionRace(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 25*time.Second)
	defer cancel()

	const (
		liveJobs          = 40
		workerCount       = 4
		workerConcurrency = 4
		retention         = 30 * 24 * time.Hour
		kind              = "jobs.stress.integration.retention"
		handlerDelay      = 10 * time.Millisecond
	)

	queue := NewQueue(pgtest.Scoped(pool))

	insertTerminal := func(jobKind, status string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at)
			VALUES ($1, '{}'::jsonb, $2, now(), 3,
			        now() - interval '40 days', now() - interval '40 days')
			RETURNING id`, jobKind, status).Scan(&id))
		return id
	}

	oldTerminalIDs := make([]int64, 0, liveJobs)
	for i := 0; i < liveJobs/2; i++ {
		oldTerminalIDs = append(oldTerminalIDs, insertTerminal(kind+".old_succeeded", "succeeded"))
		oldTerminalIDs = append(oldTerminalIDs, insertTerminal(kind+".old_dead", "dead"))
	}

	var oldQueuedID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at)
		VALUES ($1, '{}'::jsonb, 'queued', now() + interval '1 day', 3,
		        now() - interval '40 days', now() - interval '40 days')
		RETURNING id`, kind+".old_queued").Scan(&oldQueuedID))

	var oldRunningID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, status, run_at, max_attempts,
		                  created_at, updated_at, attempts, locked_at, locked_by, lease_expires_at)
		VALUES ($1, '{}'::jsonb, 'running', now(), 3,
		        now() - interval '40 days', now() - interval '40 days',
		        1, now(), 'pre-existing-worker', now() + interval '1 hour')
		RETURNING id`, kind+".old_running").Scan(&oldRunningID))

	for i := 0; i < liveJobs; i++ {
		_, err := queue.Enqueue(ctx, Job{
			Kind:        kind,
			Payload:     json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i)),
			MaxAttempts: 3,
		})
		require.NoError(t, err)
	}

	started := make(chan struct{}, liveJobs)
	firstPurgeDone := make(chan struct{})
	var (
		handledMu sync.Mutex
		handled   = make(map[int64]int, liveJobs)
	)
	handler := func(ctx context.Context, payload json.RawMessage) error {
		var item struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}

		handledMu.Lock()
		handled[int64(item.Seq)]++
		handledMu.Unlock()
		started <- struct{}{}

		// Hold every handler until the concurrent purge has completed at
		// least one pass, guaranteeing the purge observes running and queued
		// rows from the same workload.
		select {
		case <-firstPurgeDone:
		case <-ctx.Done():
			return ctx.Err()
		}
		time.Sleep(handlerDelay)
		return nil
	}

	stops := make([]func(), 0, workerCount)
	for i := 0; i < workerCount; i++ {
		worker := NewWorker(pgtest.Scoped(pool), fmt.Sprintf("stress-retention-%d", i),
			WithConcurrency(workerConcurrency),
			WithPollInterval(5*time.Millisecond),
			WithRetention(0),
			WithLeaseDuration(30*time.Second),
			WithHeartbeatInterval(5*time.Second),
		)
		worker.Register(kind, handler)
		stops = append(stops, startStressWorker(t, worker))
	}

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("no handler started before timeout")
	}

	purgeStop := make(chan struct{})
	purgeDone := make(chan struct{})
	var purgedTotal atomic.Int64
	var purgeErrs []error
	go func() {
		defer close(purgeDone)
		first := true
		for {
			select {
			case <-purgeStop:
				return
			default:
			}

			purged, err := queue.PurgeTerminal(ctx, retention)
			purgedTotal.Add(purged)
			if err != nil {
				purgeErrs = append(purgeErrs, err)
			}
			if first {
				first = false
				close(firstPurgeDone)
			}

			select {
			case <-purgeStop:
				return
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	select {
	case <-firstPurgeDone:
	case <-ctx.Done():
		close(purgeStop)
		<-purgeDone
		t.Fatal("purge did not complete a pass before timeout")
	}

	// One handler already signalled before the purge started; wait for the
	// rest of the live workload to be handed out.
	for i := 0; i < liveJobs-1; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			close(purgeStop)
			<-purgeDone
			t.Fatalf("started %d/%d live jobs before timeout", i+1, liveJobs)
		}
	}

	close(purgeStop)
	<-purgeDone
	for _, stop := range stops {
		stop()
	}
	require.Empty(t, purgeErrs, "concurrent purges must not fail")

	assert.Equal(t, int64(len(oldTerminalIDs)), purgedTotal.Load(),
		"the purge must delete every old terminal row exactly once")

	var remainingOldTerminal int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE id = ANY($1)`, oldTerminalIDs).Scan(&remainingOldTerminal))
	assert.Zero(t, remainingOldTerminal, "old terminal rows must be purged")

	var oldQueuedStatus, oldRunningStatus string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id = $1`, oldQueuedID).Scan(&oldQueuedStatus))
	assert.Equal(t, "queued", oldQueuedStatus, "an old queued row must never be purged")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id = $1`, oldRunningID).Scan(&oldRunningStatus))
	assert.Equal(t, "running", oldRunningStatus, "an old running row must never be purged")

	var succeededCount int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs WHERE kind = $1 AND status = 'succeeded'`, kind).
		Scan(&succeededCount))
	assert.Equal(t, int64(liveJobs), succeededCount,
		"every live job must still exist and succeed after the purge race")

	handledMu.Lock()
	require.Len(t, handled, liveJobs)
	for seq, count := range handled {
		assert.Equal(t, 1, count, "live job %d must run exactly once", seq)
	}
	handledMu.Unlock()
}
