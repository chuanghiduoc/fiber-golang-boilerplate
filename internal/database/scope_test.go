package database

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestScopeArgs(t *testing.T) {
	cases := []struct {
		name       string
		scope      Scope
		wantTenant string
	}{
		{name: "tenant", scope: Scope{TenantID: testID(42)}, wantTenant: testID(42).String()},
		{name: "bypass still carries its tenant", scope: Scope{TenantID: testID(42), Bypass: true}, wantTenant: testID(42).String()},
		{name: "no tenant fails closed", scope: Scope{}, wantTenant: ""},
		{name: "nil tenant fails closed", scope: Scope{TenantID: uuid.Nil}, wantTenant: ""},
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
	resolve := Resolver(func(ctx context.Context) uuid.UUID {
		if id, ok := ctx.Value(tenantKey{}).(uuid.UUID); ok {
			return id
		}
		return uuid.Nil
	})
	ctx := context.WithValue(context.Background(), tenantKey{}, testID(7))
	if got := resolve(ctx); got.TenantID != testID(7) || got.Bypass {
		t.Fatalf("Resolver() = %+v, want tenant 7 without bypass", got)
	}
	if got := resolve(WithRLSBypass(ctx)); got.TenantID != testID(7) || !got.Bypass {
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
