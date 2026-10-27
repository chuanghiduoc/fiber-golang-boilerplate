//go:build integration

package migrate

import (
	"context"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// TestCheckRLSPredicateIntegration proves the verifier reads the installed
// function, not a fixed string: a predicate that consults the old bypass GUC
// is reported and a fresh baseline passes.
func TestCheckRLSPredicateIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	owner := pgtest.New(t).Owner

	var schema string
	if err := owner.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}

	gaps, err := checkRLSPredicate(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLSPredicate on the baseline: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("baseline gaps = %v, want none", gaps)
	}

	if _, err := owner.Exec(ctx, `
		CREATE OR REPLACE FUNCTION app_tenant_visible(row_tenant UUID) RETURNS BOOLEAN
		LANGUAGE sql STABLE AS $$
		  SELECT current_setting('app.rls_bypass', true) = 'on'
		      OR row_tenant = NULLIF(current_setting('app.tenant_id', true), '')::UUID
		$$`); err != nil {
		t.Fatalf("install bypass predicate: %v", err)
	}
	gaps, err = checkRLSPredicate(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLSPredicate with a bypass predicate: %v", err)
	}
	if !hasGap(gaps, "must not read app.rls_bypass") {
		t.Fatalf("gaps = %v, want an app.rls_bypass refusal", gaps)
	}
}
