//go:build integration

package migrate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// TestCheckRLSIntegrationRejectsUnprotectedTenantTable proves the schema guard
// follows the catalog, not a static list: a new tenant_id table without RLS is
// refused, a policy that ignores app_tenant_visible is refused, and a second
// permissive policy is refused.
func TestCheckRLSIntegrationRejectsUnprotectedTenantTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	owner := pgtest.New(t).Owner

	var schema string
	if err := owner.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}

	gaps, err := checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS on the baseline: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("baseline checkRLS gaps = %v, want none", gaps)
	}

	if _, err := owner.Exec(ctx,
		`CREATE TABLE tenant_probe (id BIGSERIAL PRIMARY KEY, tenant_id UUID NOT NULL)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	gaps, err = checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS with an unprotected table: %v", err)
	}
	if !hasGap(gaps, "row-level security on tenant_probe") {
		t.Fatalf("gaps = %v, want an unprotected tenant_probe", gaps)
	}

	for _, statement := range []string{
		`ALTER TABLE tenant_probe ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE tenant_probe FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY tenant_isolation ON tenant_probe USING (true) WITH CHECK (true)`,
	} {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	gaps, err = checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS with a permissive policy: %v", err)
	}
	if !hasGap(gaps, "must reference app_tenant_visible") {
		t.Fatalf("gaps = %v, want a policy that ignores app_tenant_visible", gaps)
	}

	// A correct WITH CHECK clause does not make a read-all USING clause safe.
	if _, err := owner.Exec(ctx, `DROP POLICY tenant_isolation ON tenant_probe`); err != nil {
		t.Fatalf("drop permissive policy: %v", err)
	}
	if _, err := owner.Exec(ctx, `CREATE POLICY tenant_isolation ON tenant_probe
		USING (true) WITH CHECK (app_tenant_visible(tenant_id))`); err != nil {
		t.Fatalf("create read-all policy: %v", err)
	}
	gaps, err = checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS with a read-all USING clause: %v", err)
	}
	if !hasGap(gaps, "USING clause must reference app_tenant_visible") {
		t.Fatalf("gaps = %v, want the permissive USING clause reported", gaps)
	}

	if _, err := owner.Exec(ctx,
		`DROP POLICY tenant_isolation ON tenant_probe`); err != nil {
		t.Fatalf("drop permissive policy: %v", err)
	}
	for _, statement := range []string{
		`CREATE POLICY tenant_isolation ON tenant_probe
		     USING (app_tenant_visible(tenant_id)) WITH CHECK (app_tenant_visible(tenant_id))`,
		`CREATE POLICY extra_reader ON tenant_probe USING (true)`,
	} {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	gaps, err = checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS with an extra policy: %v", err)
	}
	if !hasGap(gaps, "single tenant_isolation policy") {
		t.Fatalf("gaps = %v, want the extra policy reported", gaps)
	}

	if _, err := owner.Exec(ctx, `DROP POLICY extra_reader ON tenant_probe`); err != nil {
		t.Fatalf("drop extra policy: %v", err)
	}
	gaps, err = checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS after the fix: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("gaps after the fix = %v, want none", gaps)
	}
}

func hasGap(gaps []string, want string) bool {
	for _, gap := range gaps {
		if strings.Contains(gap, want) {
			return true
		}
	}
	return false
}

// TestCheckRLSIntegrationCoversAuditPartitions pins the partition rule: a
// direct read of a partition skips the parent's policy, so every audit_events
// partition (the default one from the init migration and the monthly ones
// cmd/migrate creates) carries its own FORCE RLS and tenant_isolation policy,
// and the guard reports a partition that lost it.
func TestCheckRLSIntegrationCoversAuditPartitions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	owner := pgtest.New(t).Owner
	var schema string
	if err := owner.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current schema: %v", err)
	}

	if err := EnsureAuditPartitions(ctx, owner); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if err := EnsureAuditPartitions(ctx, owner); err != nil {
		t.Fatalf("ensure partitions is idempotent: %v", err)
	}
	gaps, err := checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS with monthly partitions: %v", err)
	}
	if len(gaps) != 0 {
		t.Fatalf("partition gaps = %v, want none", gaps)
	}

	if _, err := owner.Exec(ctx, `ALTER TABLE audit_events_default NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatalf("weaken default partition: %v", err)
	}
	gaps, err = checkRLS(ctx, owner, schema)
	if err != nil {
		t.Fatalf("checkRLS with a weakened partition: %v", err)
	}
	if !hasGap(gaps, "row-level security on audit_events_default") {
		t.Fatalf("gaps = %v, want the unforced default partition reported", gaps)
	}
}
