package app

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TestRLSResolverFailsClosed proves an unset tenant never inherits one:
// only a tenant stored in the context becomes the scope.
// Background work must set it explicitly (the job worker restores the job's
// tenant_id) or mark the context for the deliberate bypass pool.
func TestRLSResolverFailsClosed(t *testing.T) {
	if scope := RLSResolver(context.Background()); scope.TenantID != uuid.Nil || scope.Bypass {
		t.Fatalf("RLSResolver(empty context) = %+v, want tenant 0 without bypass", scope)
	}

	ctx := tenant.WithID(context.Background(), testID(42))
	if scope := RLSResolver(ctx); scope.TenantID != testID(42) || scope.Bypass {
		t.Fatalf("RLSResolver(tenant 42) = %+v, want tenant 42 without bypass", scope)
	}

	bypass := RLSResolver(database.WithRLSBypass(ctx))
	if bypass.TenantID != testID(42) || !bypass.Bypass {
		t.Fatalf("RLSResolver(bypass) = %+v, want tenant 42 with bypass", bypass)
	}
}
