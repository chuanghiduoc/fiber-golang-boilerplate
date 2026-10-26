package database

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestScopeArgs(t *testing.T) {
	cases := []struct {
		name       string
		scope      Scope
		wantTenant string
	}{
		{name: "tenant", scope: Scope{TenantID: 42}, wantTenant: "42"},
		{name: "bypass still carries its tenant", scope: Scope{TenantID: 42, Bypass: true}, wantTenant: "42"},
		{name: "no tenant fails closed", scope: Scope{}, wantTenant: ""},
		{name: "negative tenant fails closed", scope: Scope{TenantID: -1}, wantTenant: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeArgs(tc.scope); got != tc.wantTenant {
				t.Fatalf("scopeArgs(%+v) = %q, want %q", tc.scope, got, tc.wantTenant)
			}
		})
	}
}

func TestResolverReadsTenantAndBypass(t *testing.T) {
	resolve := Resolver(func(ctx context.Context) int64 {
		if id, ok := ctx.Value(tenantKey{}).(int64); ok {
			return id
		}
		return 0
	})
	ctx := context.WithValue(context.Background(), tenantKey{}, int64(7))
	if got := resolve(ctx); got.TenantID != 7 || got.Bypass {
		t.Fatalf("Resolver() = %+v, want tenant 7 without bypass", got)
	}
	if got := resolve(WithRLSBypass(ctx)); got.TenantID != 7 || !got.Bypass {
		t.Fatalf("Resolver(bypass) = %+v, want tenant 7 with bypass", got)
	}
}

func TestPoolsForBypass(t *testing.T) {
	primary := &pgxpool.Pool{}
	bypass := &pgxpool.Pool{}
	pools := Pools{Primary: primary, Bypass: bypass}

	if pool, gotBypass := pools.forBypass(false); pool != primary || gotBypass {
		t.Fatalf("forBypass(false) = %v/%v, want the primary pool", pool, gotBypass)
	}
	if pool, gotBypass := pools.forBypass(true); pool != bypass || !gotBypass {
		t.Fatalf("forBypass(true) = %v/%v, want the bypass pool", pool, gotBypass)
	}

	fallback := Pools{Primary: primary}
	if pool, gotBypass := fallback.forBypass(true); pool != primary || gotBypass {
		t.Fatalf("forBypass(true) without a bypass pool = %v/%v, want the primary pool", pool, gotBypass)
	}
}

type tenantKey struct{}
