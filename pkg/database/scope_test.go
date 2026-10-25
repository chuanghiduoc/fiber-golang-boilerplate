package database

import (
	"context"
	"testing"
)

func TestScopeArgs(t *testing.T) {
	cases := []struct {
		name       string
		scope      Scope
		wantTenant string
		wantBypass string
	}{
		{name: "tenant", scope: Scope{TenantID: 42}, wantTenant: "42", wantBypass: "off"},
		{name: "bypass", scope: Scope{TenantID: 42, Bypass: true}, wantTenant: "42", wantBypass: "on"},
		{name: "no tenant fails closed", scope: Scope{}, wantTenant: "", wantBypass: "off"},
		{name: "negative tenant fails closed", scope: Scope{TenantID: -1}, wantTenant: "", wantBypass: "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, bypass := scopeArgs(tc.scope)
			if tenant != tc.wantTenant || bypass != tc.wantBypass {
				t.Fatalf("scopeArgs(%+v) = %q/%q, want %q/%q", tc.scope, tenant, bypass, tc.wantTenant, tc.wantBypass)
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

type tenantKey struct{}
