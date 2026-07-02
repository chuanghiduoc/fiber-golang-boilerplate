package health

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

// Status represents a health check result.
type Status struct {
	Status  string            `json:"status"`
	Details map[string]string `json:"details,omitempty"`
}

// Checker aggregates health checks for all dependencies.
type Checker struct {
	pool  *pgxpool.Pool
	cache cache.Cache
}

// NewChecker creates a new health checker.
func NewChecker(pool *pgxpool.Pool, appCache cache.Cache) *Checker {
	return &Checker{pool: pool, cache: appCache}
}

// Liveness returns basic liveness (process is running).
func (h *Checker) Liveness() Status {
	return Status{Status: "up"}
}

// Readiness checks all dependencies are ready.
func (h *Checker) Readiness(ctx context.Context) Status {
	details := make(map[string]string)
	allUp := true

	var mu sync.Mutex
	var wg sync.WaitGroup

	// set records a check result under the mutex, keeping the pings themselves
	// concurrent (the lock only guards the shared map and allUp flag).
	set := func(key, value string, up bool) {
		mu.Lock()
		defer mu.Unlock()
		details[key] = value
		if !up {
			allUp = false
		}
	}

	// Check database
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.pool.Ping(ctx); err != nil {
			set("database", fmt.Sprintf("down: %v", err), false)
			return
		}
		stats := h.pool.Stat()
		mu.Lock()
		defer mu.Unlock()
		details["database"] = "up"
		details["db_total_conns"] = strconv.Itoa(int(stats.TotalConns()))
		details["db_idle_conns"] = strconv.Itoa(int(stats.IdleConns()))
	}()

	// Check cache
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := h.cache.Ping(ctx); err != nil {
			set("cache", fmt.Sprintf("down: %v", err), false)
			return
		}
		set("cache", "up", true)
	}()

	wg.Wait()

	status := "up"
	if !allUp {
		status = "degraded"
	}
	return Status{Status: status, Details: details}
}
