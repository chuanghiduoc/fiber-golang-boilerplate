package features

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// Default cache timing. A short TTL bounds how long a replica can serve an
// outdated snapshot; MaxStaleness bounds how long the last good snapshot may
// be used while the database is unreachable.
const (
	DefaultCacheTTL     = 30 * time.Second
	DefaultMaxStaleness = 5 * time.Minute
)

// Source identifies which precedence rule produced an evaluation result.
type Source string

const (
	SourceForceOff        Source = "force_off"
	SourceKillSwitch      Source = "kill_switch"
	SourceUserTarget      Source = "user_target"
	SourceTenantTarget    Source = "tenant_target"
	SourceDatabaseDefault Source = "database_default"
	SourceEnvironment     Source = "environment"
	SourceCompiledDefault Source = "compiled_default"
)

// Result is a single evaluated feature flag.
type Result struct {
	Enabled bool
	Source  Source
}

// Subject identifies the caller whose overrides should apply. A zero TenantID
// resolves from the context (falling back to the compatibility tenant), and a
// zero UserID skips user-target lookup.
type Subject struct {
	TenantID int64
	UserID   int64
}

// SubjectFromContext builds a subject from the request tenant and an
// authenticated user ID.
func SubjectFromContext(ctx context.Context, userID int64) Subject {
	return Subject{TenantID: tenant.EffectiveID(ctx), UserID: userID}
}

// FlagState is the database-level state for one flag.
type FlagState struct {
	Enabled    bool
	KillSwitch bool
}

// UserTargetKey identifies a user override inside one tenant.
type UserTargetKey struct {
	TenantID int64
	UserID   int64
}

// Snapshot is the complete flag state loaded from the optional database
// store. A zero LoadedAt is filled in by the evaluator.
type Snapshot struct {
	Flags         map[Key]FlagState
	TenantTargets map[Key]map[int64]bool
	UserTargets   map[Key]map[UserTargetKey]bool
	LoadedAt      time.Time
}

// Store loads a full snapshot. Implementations should return every flag and
// target; the evaluator filters by tenant and user in memory.
type Store interface {
	LoadSnapshot(ctx context.Context) (Snapshot, error)
}

// StoreFunc adapts a function to Store for tests and small integrations.
type StoreFunc func(ctx context.Context) (Snapshot, error)

// LoadSnapshot implements Store.
func (f StoreFunc) LoadSnapshot(ctx context.Context) (Snapshot, error) {
	return f(ctx)
}

// Evaluator resolves feature flags. It is safe for concurrent use.
type Evaluator interface {
	// Enabled returns the resolved boolean value.
	Enabled(ctx context.Context, key Key, subject Subject) bool
	// Evaluate returns the value and the precedence rule that produced it.
	Evaluate(ctx context.Context, key Key, subject Subject) Result
	// Invalidate drops the cached snapshot. The next evaluation reloads from
	// the store. Other replicas converge when their cache TTL expires.
	Invalidate(ctx context.Context) error
}

// Options configures a CachedEvaluator. Store may be nil, in which case only
// force-off, environment, and compiled defaults apply.
type Options struct {
	Store        Store
	Env          map[Key]bool
	ForceOff     map[Key]struct{}
	CacheTTL     time.Duration
	MaxStaleness time.Duration
	// Now is injectable for tests; it defaults to time.Now.
	Now func() time.Time
}

// CachedEvaluator caches one database snapshot in process memory. The mutex
// protects the snapshot; refreshMu serialises database loads so a cold cache
// does not cause a thundering herd.
type CachedEvaluator struct {
	store        Store
	env          map[Key]bool
	forceOff     map[Key]struct{}
	cacheTTL     time.Duration
	maxStaleness time.Duration
	now          func() time.Time

	mu          sync.RWMutex
	snapshot    *Snapshot
	loadedAt    time.Time
	lastAttempt time.Time
	generation  uint64
	refreshMu   sync.Mutex
}

// NewEvaluator builds an evaluator. Invalid or zero timings fall back to the
// package defaults, and MaxStaleness is never allowed below CacheTTL.
func NewEvaluator(opts Options) Evaluator {
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	maxStaleness := opts.MaxStaleness
	if maxStaleness <= 0 {
		maxStaleness = DefaultMaxStaleness
	}
	if maxStaleness < ttl {
		maxStaleness = ttl
	}

	env := make(map[Key]bool, len(opts.Env))
	for key, enabled := range opts.Env {
		env[key] = enabled
	}
	forceOff := make(map[Key]struct{}, len(opts.ForceOff))
	for key := range opts.ForceOff {
		forceOff[key] = struct{}{}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &CachedEvaluator{
		store:        opts.Store,
		env:          env,
		forceOff:     forceOff,
		cacheTTL:     ttl,
		maxStaleness: maxStaleness,
		now:          now,
	}
}

// Enabled implements Evaluator.
func (e *CachedEvaluator) Enabled(ctx context.Context, key Key, subject Subject) bool {
	return e.Evaluate(ctx, key, subject).Enabled
}

// Evaluate implements Evaluator. Precedence, highest first:
// force-off > kill switch > user target > tenant target > database default >
// environment > compiled default.
func (e *CachedEvaluator) Evaluate(ctx context.Context, key Key, subject Subject) Result {
	if _, disabled := e.forceOff[key]; disabled {
		e.record(key, SourceForceOff, false)
		return Result{Enabled: false, Source: SourceForceOff}
	}

	subject = normalizeSubject(ctx, subject)
	snapshot := e.snapshotFor(ctx)
	enabled, source := e.resolve(key, subject, snapshot)
	e.record(key, source, enabled)
	return Result{Enabled: enabled, Source: source}
}

func normalizeSubject(ctx context.Context, subject Subject) Subject {
	if subject.TenantID == 0 {
		subject.TenantID = tenant.EffectiveID(ctx)
	}
	if subject.UserID < 0 {
		subject.UserID = 0
	}
	return subject
}

func (e *CachedEvaluator) resolve(key Key, subject Subject, snapshot *Snapshot) (bool, Source) {
	if snapshot != nil {
		if flag, ok := snapshot.Flags[key]; ok {
			if flag.KillSwitch {
				return false, SourceKillSwitch
			}
			if subject.UserID != 0 {
				if targets := snapshot.UserTargets[key]; targets != nil {
					if enabled, ok := targets[UserTargetKey(subject)]; ok {
						return enabled, SourceUserTarget
					}
				}
			}
			if targets := snapshot.TenantTargets[key]; targets != nil {
				if enabled, ok := targets[subject.TenantID]; ok {
					return enabled, SourceTenantTarget
				}
			}
			return flag.Enabled, SourceDatabaseDefault
		}
	}

	if enabled, ok := e.env[key]; ok {
		return enabled, SourceEnvironment
	}
	return CompiledDefault(key), SourceCompiledDefault
}

// snapshotFor returns a usable snapshot or nil when the database has never
// loaded or the last good snapshot is older than MaxStaleness.
func (e *CachedEvaluator) snapshotFor(ctx context.Context) *Snapshot {
	if e.store == nil {
		return nil
	}

	now := e.now()
	if snapshot, age, ok := e.freshSnapshot(now); ok {
		metrics.FeatureFlagCacheHitsTotal.Inc()
		metrics.FeatureFlagSnapshotAgeSeconds.Set(age.Seconds())
		return snapshot
	}

	metrics.FeatureFlagCacheMissesTotal.Inc()

	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	for {
		// Another goroutine may have refreshed (or invalidated) while we
		// waited for the refresh lock.
		now = e.now()
		if snapshot, age, ok := e.freshSnapshot(now); ok {
			metrics.FeatureFlagCacheHitsTotal.Inc()
			metrics.FeatureFlagSnapshotAgeSeconds.Set(age.Seconds())
			return snapshot
		}

		e.mu.RLock()
		lastAttempt := e.lastAttempt
		staleSnapshot := e.snapshot
		staleAt := e.loadedAt
		generation := e.generation
		e.mu.RUnlock()

		// A failed load is throttled for one TTL so an unavailable database
		// does not turn every request into a failing query.
		if !lastAttempt.IsZero() && now.Sub(lastAttempt) < e.cacheTTL && lastAttempt.After(staleAt) {
			return e.usableStale(staleSnapshot, staleAt, now)
		}

		snapshot, err := e.store.LoadSnapshot(ctx)
		attemptAt := e.now()
		if err != nil {
			metrics.FeatureFlagSnapshotLoadsTotal.WithLabelValues("error").Inc()
			slog.Warn("feature flag snapshot load failed; using fallback",
				slog.Any("error", err),
			)
			e.mu.Lock()
			e.lastAttempt = attemptAt
			e.mu.Unlock()
			return e.usableStale(staleSnapshot, staleAt, now)
		}

		snapshot.LoadedAt = attemptAt
		metrics.FeatureFlagSnapshotLoadsTotal.WithLabelValues("success").Inc()

		e.mu.Lock()
		// Drop the result if Invalidate ran while the query was in flight: the
		// snapshot may predate the mutation that triggered invalidation.
		if e.generation == generation {
			e.snapshot = &snapshot
			e.loadedAt = attemptAt
			e.lastAttempt = attemptAt
			metrics.FeatureFlagSnapshotAgeSeconds.Set(0)
			e.mu.Unlock()
			return &snapshot
		}
		e.mu.Unlock()
		// Loop and reload against the current generation.
	}
}

func (e *CachedEvaluator) freshSnapshot(now time.Time) (*Snapshot, time.Duration, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.snapshot == nil || e.loadedAt.IsZero() {
		return nil, 0, false
	}
	age := now.Sub(e.loadedAt)
	if age < 0 || age >= e.cacheTTL {
		return nil, age, false
	}
	return e.snapshot, age, true
}

func (e *CachedEvaluator) usableStale(snapshot *Snapshot, loadedAt, now time.Time) *Snapshot {
	if snapshot == nil || loadedAt.IsZero() {
		metrics.FeatureFlagSnapshotFallbackTotal.Inc()
		return nil
	}
	age := now.Sub(loadedAt)
	if age < 0 || age > e.maxStaleness {
		metrics.FeatureFlagSnapshotFallbackTotal.Inc()
		return nil
	}
	metrics.FeatureFlagSnapshotStaleTotal.Inc()
	metrics.FeatureFlagSnapshotAgeSeconds.Set(age.Seconds())
	return snapshot
}

// Invalidate implements Evaluator.
func (e *CachedEvaluator) Invalidate(context.Context) error {
	e.mu.Lock()
	e.snapshot = nil
	e.loadedAt = time.Time{}
	e.lastAttempt = time.Time{}
	e.generation++
	e.mu.Unlock()
	metrics.FeatureFlagInvalidationsTotal.Inc()
	return nil
}

func (e *CachedEvaluator) record(key Key, source Source, enabled bool) {
	metrics.FeatureFlagEvaluationsTotal.WithLabelValues(
		string(key), string(source), strconv.FormatBool(enabled),
	).Inc()
}
