package health

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
)

// Overall readiness states. Only StatusDown should take a pod out of rotation.
const (
	StatusUp       = "up"
	StatusDegraded = "degraded"
	StatusDown     = "down"
)

// Per-dependency states reported in Details.
const (
	checkUp   = "up"
	checkBusy = "busy"
	checkDown = "down"
)

const checkTimeout = 2 * time.Second

// readinessCacheTTL is how long one readiness result is reused. /readyz and
// /health are unauthenticated; without it every request would ping the
// database and cache, so a flood could exhaust the pool. A second is far
// below any probe period, so probes still see fresh state.
const readinessCacheTTL = time.Second

// Status represents a health check result. Details carries only coarse
// per-dependency states; pool sizes and other internals are never exposed on
// the public probe endpoints.
type Status struct {
	Status  string            `json:"status"`
	Details map[string]string `json:"details,omitempty"`
}

// Checker aggregates health checks for all dependencies.
type Checker struct {
	// pingDB acquires a pooled connection and pings it.
	pingDB func(context.Context) error
	// dbSaturated reports whether every pool connection is checked out.
	dbSaturated func() bool
	cache       cache.Cache
	// now is injectable for tests; nil means time.Now.
	now func() time.Time

	// readyMu guards the cached readiness result and is held while a check
	// runs, so concurrent callers share one in-flight check.
	readyMu sync.Mutex
	ready   Status
	readyAt time.Time
}

// NewChecker creates a new health checker.
func NewChecker(pool *pgxpool.Pool, appCache cache.Cache) *Checker {
	h := &Checker{cache: appCache}
	if pool != nil {
		h.pingDB = pool.Ping
		h.dbSaturated = func() bool {
			stat := pool.Stat()
			return stat.MaxConns() > 0 && stat.AcquiredConns() >= stat.MaxConns()
		}
	}
	return h
}

// Liveness returns basic liveness (process is running).
func (h *Checker) Liveness() Status {
	return Status{Status: StatusUp}
}

// Readiness checks dependencies concurrently and classifies the result:
//
//   - database down: StatusDown (the API cannot serve requests).
//   - database busy: the ping timed out only because every pooled connection
//     is checked out. Those connections were opened against a reachable
//     database and are serving requests, so the pod is StatusDegraded rather
//     than removed; removing every saturated pod at once would cascade load
//     onto the rest. If the database is really gone, in-flight queries fail,
//     release their connections, and the next probe reports down.
//   - cache down: StatusDegraded. Rate limiting and idempotency fail open, so
//     a cache outage must not take every replica out of rotation.
//
// A result is reused for readinessCacheTTL, and concurrent callers wait for
// the single in-flight check instead of starting their own. A check cut short
// by the caller's context is returned to that caller but not cached.
func (h *Checker) Readiness(ctx context.Context) Status {
	h.readyMu.Lock()
	defer h.readyMu.Unlock()

	if !h.readyAt.IsZero() && h.clock().Sub(h.readyAt) < readinessCacheTTL {
		return h.ready.clone()
	}
	status := h.checkReadiness(ctx)
	if ctx.Err() == nil {
		h.ready = status
		h.readyAt = h.clock()
	}
	return status.clone()
}

func (h *Checker) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (s Status) clone() Status {
	s.Details = maps.Clone(s.Details)
	return s
}

func (h *Checker) checkReadiness(ctx context.Context) Status {
	details := make(map[string]string, 2)
	var mu sync.Mutex
	var wg sync.WaitGroup
	set := func(key, value string) {
		mu.Lock()
		defer mu.Unlock()
		details[key] = value
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		set("database", h.checkDatabase(ctx))
	}()
	go func() {
		defer wg.Done()
		set("cache", h.checkCache(ctx))
	}()
	wg.Wait()

	status := StatusUp
	switch {
	case details["database"] == checkDown:
		status = StatusDown
	case details["database"] != checkUp || details["cache"] != checkUp:
		status = StatusDegraded
	}
	return Status{Status: status, Details: details}
}

func (h *Checker) checkDatabase(ctx context.Context) string {
	if h.pingDB == nil {
		slog.Error("readiness: database is not configured")
		return checkDown
	}
	dbCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	err := h.pingDB(dbCtx)
	if err == nil {
		return checkUp
	}
	if errors.Is(err, context.DeadlineExceeded) && h.dbSaturated != nil && h.dbSaturated() {
		slog.Warn("readiness: database pool saturated; reporting degraded", slog.Any("error", err))
		return checkBusy
	}
	slog.Error("readiness: database ping failed", slog.Any("error", err))
	return checkDown
}

func (h *Checker) checkCache(ctx context.Context) string {
	if h.cache == nil {
		slog.Error("readiness: cache is not configured")
		return checkDown
	}
	cacheCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := h.cache.Ping(cacheCtx); err != nil {
		slog.Error("readiness: cache ping failed", slog.Any("error", err))
		return checkDown
	}
	return checkUp
}
