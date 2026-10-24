package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

func TestLiveness(t *testing.T) {
	checker := &Checker{}
	status := checker.Liveness()

	if status.Status != "up" {
		t.Errorf("Liveness().Status = %q, want up", status.Status)
	}
}

func TestNewChecker(t *testing.T) {
	checker := NewChecker(nil, nil)
	if checker == nil {
		t.Fatal("NewChecker returned nil")
	}
	if checker.pingDB != nil {
		t.Error("expected no database probe for a nil pool")
	}
}

func TestNewChecker_Liveness(t *testing.T) {
	checker := NewChecker(nil, nil)
	status := checker.Liveness()
	if status.Status != "up" {
		t.Errorf("Liveness().Status = %q, want up", status.Status)
	}
}

type pingCache struct {
	cache.Cache
	err error
}

func (p pingCache) Ping(context.Context) error { return p.err }

func TestReadiness(t *testing.T) {
	healthyCache := pingCache{}
	brokenCache := pingCache{err: errors.New("redis: connection refused")}
	pingOK := func(context.Context) error { return nil }
	pingTimeout := func(context.Context) error {
		return errors.Join(errors.New("acquire"), context.DeadlineExceeded)
	}
	pingRefused := func(context.Context) error { return errors.New("dial tcp: connection refused") }
	saturated := func() bool { return true }
	idle := func() bool { return false }

	tests := []struct {
		name       string
		checker    *Checker
		wantStatus string
		wantDB     string
		wantCache  string
	}{
		{"all up", &Checker{pingDB: pingOK, dbSaturated: idle, cache: healthyCache}, StatusUp, "up", "up"},
		{"cache down is degraded", &Checker{pingDB: pingOK, dbSaturated: idle, cache: brokenCache}, StatusDegraded, "up", "down"},
		{"saturated pool is degraded", &Checker{pingDB: pingTimeout, dbSaturated: saturated, cache: healthyCache}, StatusDegraded, "busy", "up"},
		{"timeout with idle pool is down", &Checker{pingDB: pingTimeout, dbSaturated: idle, cache: healthyCache}, StatusDown, "down", "up"},
		{"refused is down even when saturated", &Checker{pingDB: pingRefused, dbSaturated: saturated, cache: healthyCache}, StatusDown, "down", "up"},
		{"no database configured is down", &Checker{cache: healthyCache}, StatusDown, "down", "up"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.checker.Readiness(context.Background())
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.Details["database"] != tt.wantDB || got.Details["cache"] != tt.wantCache {
				t.Errorf("Details = %v, want database=%s cache=%s", got.Details, tt.wantDB, tt.wantCache)
			}
			if len(got.Details) != 2 {
				t.Errorf("Details = %v, must only contain database and cache states (no pool stats)", got.Details)
			}
		})
	}
}

func TestReadiness_BoundsSlowChecks(t *testing.T) {
	block := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	checker := &Checker{pingDB: block, dbSaturated: func() bool { return false }, cache: pingCache{}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got := checker.Readiness(ctx); got.Status != StatusDown {
		t.Errorf("Status = %q, want down", got.Status)
	}
}
