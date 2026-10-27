package migrate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMigrationGuardCoversTenantFoundation(t *testing.T) {
	for _, table := range []string{
		"tenants", "memberships", "invitations", "sessions", "refresh_tokens",
		"files", "jobs", "audit_events",
	} {
		if table == "tenants" {
			continue
		}
		if !slices.Contains(requiredRLS, table) {
			t.Errorf("%s must be part of the RLS guard", table)
		}
	}
	for table, triggers := range requiredTriggers {
		if len(triggers) == 0 {
			t.Errorf("%s has no trigger guard", table)
		}
	}
	if !slices.Contains(requiredTriggers["invitations"], "trigger_invitations_role_scope") {
		t.Error("invitation role scope trigger must be verified")
	}
}

// TestRLSGaps verifies the decision for every shape a tenant-owned table can
// have. The SQL only feeds it rows; the policy rules are testable without a
// database.
func TestRLSGaps(t *testing.T) {
	goodPolicy := rlsPolicy{
		Name:  "tenant_isolation",
		Using: "app_tenant_visible(tenant_id)",
		Check: "app_tenant_visible(tenant_id)",
	}
	required := func() []rlsTable {
		tables := make([]rlsTable, 0, len(requiredRLS))
		for _, table := range requiredRLS {
			tables = append(tables, rlsTable{
				Table:    table,
				Enabled:  true,
				Forced:   true,
				Policies: []rlsPolicy{goodPolicy},
			})
		}
		return tables
	}

	cases := []struct {
		name   string
		mutate func([]rlsTable) []rlsTable
		want   string
	}{
		{name: "baseline", mutate: func(tables []rlsTable) []rlsTable { return tables }},
		{name: "not enabled", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Enabled = false
			return tables
		}, want: "row-level security on " + requiredRLS[0]},
		{name: "not forced", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Forced = false
			return tables
		}, want: "forced row-level security on " + requiredRLS[0]},
		{name: "no policy", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = nil
			return tables
		}, want: "tenant_isolation policy on " + requiredRLS[0]},
		{name: "wrong policy name", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = []rlsPolicy{{Name: "other", Using: "app_tenant_visible(tenant_id)"}}
			return tables
		}, want: "tenant_isolation policy on " + requiredRLS[0]},
		{name: "policy does not reference the predicate", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = []rlsPolicy{{Name: "tenant_isolation", Using: "true", Check: "true"}}
			return tables
		}, want: "must reference app_tenant_visible"},
		{name: "permissive USING with a correct check clause", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = []rlsPolicy{{
				Name:  "tenant_isolation",
				Using: "true",
				Check: "app_tenant_visible(tenant_id)",
			}}
			return tables
		}, want: "USING clause must reference app_tenant_visible"},
		{name: "permissive WITH CHECK with a correct USING clause", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = []rlsPolicy{{
				Name:  "tenant_isolation",
				Using: "app_tenant_visible(tenant_id)",
				Check: "true",
			}}
			return tables
		}, want: "WITH CHECK clause must reference app_tenant_visible"},
		{name: "USING only (WITH CHECK defaults to USING)", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = []rlsPolicy{{
				Name:  "tenant_isolation",
				Using: "app_tenant_visible(tenant_id)",
			}}
			return tables
		}},
		{name: "extra policy", mutate: func(tables []rlsTable) []rlsTable {
			tables[0].Policies = append(tables[0].Policies, rlsPolicy{Name: "extra", Using: "true"})
			return tables
		}, want: "single tenant_isolation policy"},
		{name: "required table missing from the dynamic result", mutate: func(tables []rlsTable) []rlsTable {
			return tables[1:]
		}, want: "row-level security on " + requiredRLS[0]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gaps := rlsGaps(tc.mutate(required()))
			if tc.want == "" {
				if len(gaps) != 0 {
					t.Fatalf("rlsGaps() = %v, want none", gaps)
				}
				return
			}
			if len(gaps) != 1 || !strings.Contains(gaps[0], tc.want) {
				t.Fatalf("rlsGaps() = %v, want one gap containing %q", gaps, tc.want)
			}
		})
	}
}

func TestSchemaSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.snapshot.sql")
	raw := "-- comment\n" +
		"table\ttenants\t\"relkind=r\"\n" +
		"column\ttenants.id\t\"uuid notnull=true default=uuidv7() identity=\"\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	entries, err := readSchemaSnapshot(path)
	if err != nil {
		t.Fatalf("readSchemaSnapshot: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	got := entries["column\x00tenants.id"]
	if got.Definition != "uuid notnull=true default=uuidv7() identity=" {
		t.Fatalf("definition = %q", got.Definition)
	}
}

func TestSchemaSnapshotRejectsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.snapshot.sql")
	if err := os.WriteFile(path, []byte("table\ttenants\n"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	if _, err := readSchemaSnapshot(path); err == nil {
		t.Fatal("a malformed snapshot line must be rejected")
	}
}
