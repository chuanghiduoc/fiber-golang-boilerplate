package main

import (
	"strings"
	"testing"
)

func TestRLSPredicateGaps(t *testing.T) {
	safe := `CREATE OR REPLACE FUNCTION public.app_tenant_visible(row_tenant bigint) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT row_tenant = NULLIF(current_setting('app.tenant_id'::text, true), ''::text)::bigint $$`
	if gaps := rlsPredicateGaps(safe); len(gaps) != 0 {
		t.Fatalf("safe predicate gaps = %v, want none", gaps)
	}

	withBypass := strings.Replace(safe,
		"SELECT row_tenant =",
		"SELECT current_setting('app.rls_bypass', true) = 'on' OR row_tenant =", 1)
	gaps := rlsPredicateGaps(withBypass)
	if !containsGap(gaps, "must not read app.rls_bypass") {
		t.Fatalf("gaps = %v, want an app.rls_bypass refusal", gaps)
	}

	withoutTenant := strings.Replace(safe, "app.tenant_id", "app.other", 1)
	gaps = rlsPredicateGaps(withoutTenant)
	if !containsGap(gaps, "must compare the row tenant with app.tenant_id") {
		t.Fatalf("gaps = %v, want an app.tenant_id refusal", gaps)
	}
}

func containsGap(gaps []string, want string) bool {
	for _, gap := range gaps {
		if strings.Contains(gap, want) {
			return true
		}
	}
	return false
}
