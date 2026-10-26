package app

import (
	"context"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TestRLSResolverFailsClosed proves an unset tenant never inherits the
// compatibility tenant: only a tenant stored in the context becomes the scope.
// Background work must set it explicitly (the job worker restores the job's
// tenant_id) or mark the context for the deliberate bypass pool.
func TestRLSResolverFailsClosed(t *testing.T) {
	if scope := RLSResolver(context.Background()); scope.TenantID != 0 || scope.Bypass {
		t.Fatalf("RLSResolver(empty context) = %+v, want tenant 0 without bypass", scope)
	}

	ctx := tenant.WithID(context.Background(), 42)
	if scope := RLSResolver(ctx); scope.TenantID != 42 || scope.Bypass {
		t.Fatalf("RLSResolver(tenant 42) = %+v, want tenant 42 without bypass", scope)
	}

	bypass := RLSResolver(database.WithRLSBypass(ctx))
	if bypass.TenantID != 42 || !bypass.Bypass {
		t.Fatalf("RLSResolver(bypass) = %+v, want tenant 42 with bypass", bypass)
	}
}
