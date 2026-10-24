// Package jobs provides a small PostgreSQL-backed durable job queue and
// worker. Jobs are stored in the jobs table, claimed with FOR UPDATE SKIP
// LOCKED, and retried with exponential backoff until they succeed or exhaust
// their attempt budget. A running worker renews its job lease with heartbeats;
// leases that expire (for example after a crashed process) are requeued
// automatically.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultMaxAttempts is used when Job.MaxAttempts is zero.
	DefaultMaxAttempts = 10

	defaultBackoffBase = time.Second
	defaultBackoffMax  = time.Hour
)

var (
	// ErrEmptyKind is returned when a job has no kind.
	ErrEmptyKind = errors.New("jobs: kind is required")
	// ErrInvalidPayload is returned when a job payload is not valid JSON.
	ErrInvalidPayload = errors.New("jobs: payload must be valid JSON")
	// ErrInvalidMaxAttempts is returned when Job.MaxAttempts is negative.
	ErrInvalidMaxAttempts = errors.New("jobs: max attempts must not be negative")
	// ErrNoHandler is returned internally when a claimed job kind has no
	// registered handler. Such jobs follow the normal retry path and are
	// dead-lettered once their attempt budget is exhausted.
	ErrNoHandler = errors.New("jobs: no handler registered")
	// ErrNoPool is returned when a queue or worker has no database pool.
	ErrNoPool = errors.New("jobs: database pool is nil")
	// ErrNoTx is returned when EnqueueTx is called with a nil transaction.
	ErrNoTx = errors.New("jobs: transaction is nil")
	// ErrLeaseLost is returned by Heartbeat, Succeed, or Fail when the job is
	// no longer running under the supplied worker and attempt, usually because
	// its lease expired and another worker reclaimed it.
	ErrLeaseLost = errors.New("jobs: worker lease lost")
	// ErrInvalidRetention is returned when a retention window is not positive.
	ErrInvalidRetention = errors.New("jobs: retention must be positive")
	// ErrInvalidTenant is returned when a job cannot be associated with a
	// tenant. Enqueue resolves a zero TenantID from the context, so this
	// indicates a non-positive explicit value.
	ErrInvalidTenant = errors.New("jobs: tenant id must be positive")
	// ErrEmptyIdempotencyKey is returned by EnqueueOnce when no key is given.
	// A deterministic key is required to deduplicate producer retries.
	ErrEmptyIdempotencyKey = errors.New("jobs: idempotency key is required")
	// ErrInvalidIdempotencyKey is returned when a key exceeds the index limit.
	ErrInvalidIdempotencyKey = errors.New("jobs: invalid idempotency key")
)

// maxIdempotencyKeyLength bounds a key so producer bugs cannot create
// unbounded index entries. Event fan-out keys stay well below this limit.
const maxIdempotencyKeyLength = 512

// Job describes a unit of work to enqueue.
type Job struct {
	// TenantID owns the job. Enqueue and EnqueueTx resolve a zero value from
	// the context via tenant.EffectiveID; a positive explicit value is kept.
	TenantID int64
	// Kind selects the registered handler that processes the job.
	Kind string
	// Payload is passed verbatim to the handler. Nil or empty payloads are
	// stored as an empty JSON object.
	Payload json.RawMessage
	// RunAt is the earliest time the job may be claimed. Zero means now.
	RunAt time.Time
	// MaxAttempts caps how many times the job may be claimed. Zero means
	// DefaultMaxAttempts.
	MaxAttempts int
	// IdempotencyKey, when set on EnqueueOnce or EnqueueOnceTx, suppresses a
	// duplicate insert for the same tenant and key. Enqueue and EnqueueTx keep
	// the key on the row but do not suppress conflicts, so concurrent callers
	// receive the normal unique-violation error.
	IdempotencyKey string
}

// normalize applies documented defaults and validates the job. now is passed
// in so callers can keep a consistent clock.
func (j Job) normalize(now time.Time) (Job, error) {
	if strings.TrimSpace(j.Kind) == "" {
		return Job{}, ErrEmptyKind
	}

	if len(j.Payload) == 0 {
		j.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(j.Payload) {
		return Job{}, fmt.Errorf("%w: kind %q", ErrInvalidPayload, j.Kind)
	}

	if j.MaxAttempts < 0 {
		return Job{}, fmt.Errorf("%w: got %d", ErrInvalidMaxAttempts, j.MaxAttempts)
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = DefaultMaxAttempts
	}

	j.IdempotencyKey = strings.TrimSpace(j.IdempotencyKey)
	if len(j.IdempotencyKey) > maxIdempotencyKeyLength {
		return Job{}, fmt.Errorf("%w: key exceeds %d bytes", ErrInvalidIdempotencyKey, maxIdempotencyKeyLength)
	}

	if j.RunAt.IsZero() {
		j.RunAt = now.UTC()
	}

	return j, nil
}

// backoffDelay returns the delay before the next attempt. attempt is the
// number of attempts already made (1 after the first claim), so the delay is
// base, 2*base, 4*base, ... capped at max.
func backoffDelay(attempt int) time.Duration {
	return scaledBackoff(attempt, defaultBackoffBase, defaultBackoffMax)
}

// scaledBackoff is backoffDelay with explicit bounds, which keeps it easy to
// test and reuse.
func scaledBackoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= maxDelay || delay > maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}
