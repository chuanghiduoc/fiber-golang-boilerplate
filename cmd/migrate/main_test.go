package main

import (
	"strings"
	"testing"
)

func TestRunIsDefined(t *testing.T) {
	// Compile-level guard: main must keep a testable run() entry point.
	_ = run
}

func TestMigrationGuardCoversTenantFoundation(t *testing.T) {
	for _, column := range []string{"slug", "status"} {
		if !contains(requiredColumns["tenants"], column) {
			t.Errorf("tenants.%s must be part of the migration guard", column)
		}
	}
	if !contains(requiredUniqueIndexes, "tenants_slug_key") {
		t.Error("the tenants.slug unique constraint must be verified")
	}

	// Every tenant-owned table must have at least one index whose first key is
	// tenant_id, otherwise a scoped query falls back to a full-table scan.
	for _, table := range []string{
		"users", "refresh_tokens", "password_reset_tokens",
		"email_verification_tokens", "files", "jobs", "audit_events",
	} {
		indexes := requiredTenantLeadingIndexes[table]
		if len(indexes) == 0 {
			t.Errorf("%s has no tenant-leading index guard", table)
			continue
		}
		for _, index := range indexes {
			if !strings.Contains(index, "tenant") {
				t.Errorf("%s index %q is not a tenant-leading index", table, index)
			}
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestJobsRequireTraceParentColumn(t *testing.T) {
	// Fresh installs get jobs.trace_parent from the consolidated baseline.
	// cmd/migrate must verify it so an older schema cannot silently break
	// queue inserts after deployment.
	found := false
	for _, column := range requiredColumns["jobs"] {
		if column == "trace_parent" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("requiredColumns[jobs] must include trace_parent")
	}
}
