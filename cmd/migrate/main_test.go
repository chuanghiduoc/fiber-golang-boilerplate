package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

var (
	baselineTable   = regexp.MustCompile(`(?i)\bCREATE TABLE IF NOT EXISTS ([a-z_][a-z0-9_]*)`)
	baselineIndex   = regexp.MustCompile(`(?i)\bCREATE (UNIQUE )?INDEX IF NOT EXISTS ([a-z_][a-z0-9_]*)\s+ON ([a-z_][a-z0-9_]*)\s*\(\s*([a-z_][a-z0-9_]*)`)
	baselineTrigger = regexp.MustCompile(`(?is)\bCREATE TRIGGER ([a-z_][a-z0-9_]*)\s+(?:BEFORE|AFTER|INSTEAD OF)\b[^;]*?\bON ([a-z_][a-z0-9_]*)`)
)

// implicitUniqueIndexes are created by a UNIQUE or PRIMARY KEY constraint, so
// their names never appear as CREATE INDEX statements in the baseline.
var implicitUniqueIndexes = map[string]bool{
	"tenants_slug_key":                         true,
	"uq_user_identities_provider_subject":      true,
	"uq_user_identities_user_provider":         true,
	"uq_memberships_tenant_user":               true,
	"uq_roles_tenant_key":                      true,
	"permissions_pkey":                         true,
	"role_permissions_pkey":                    true,
	"membership_roles_pkey":                    true,
	"platform_role_assignments_pkey":           true,
	"uq_sessions_tenant_id":                    true,
	"invitations_token_hash_key":               true,
	"refresh_tokens_token_hash_key":            true,
	"password_reset_tokens_user_id_key":        true,
	"password_reset_tokens_token_hash_key":     true,
	"email_verification_tokens_user_id_key":    true,
	"email_verification_tokens_token_hash_key": true,
	"api_key_scopes_pkey":                      true,
	"webhook_endpoint_subscriptions_pkey":      true,
	"webhook_events_pkey":                      true,
}

// readBaseline concatenates every up migration in version order: together
// they are the schema the guard verifies.
func readBaseline(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list up migrations: %v (found %d)", err, len(paths))
	}
	sort.Strings(paths)
	var schema strings.Builder
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		schema.Write(raw)
		schema.WriteString("\n")
	}
	return schema.String()
}

// TestRequiredObjectsMatchBaseline catches drift between the post-migration
// guard and the migration files: a table added to the schema but not
// verified, or a verified index/trigger that was renamed or dropped.
func TestRequiredObjectsMatchBaseline(t *testing.T) {
	baseline := readBaseline(t)

	matches := baselineTable.FindAllStringSubmatch(baseline, -1)
	tables := make([]string, 0, len(matches))
	for _, match := range matches {
		tables = append(tables, strings.ToLower(match[1]))
	}
	sort.Strings(tables)
	required := slices.Clone(requiredTables)
	sort.Strings(required)
	if !slices.Equal(tables, required) {
		t.Errorf("requiredTables = %v, baseline CREATE TABLE names = %v", required, tables)
	}

	type index struct {
		unique             bool
		table, firstColumn string
	}
	indexes := map[string]index{}
	for _, match := range baselineIndex.FindAllStringSubmatch(baseline, -1) {
		indexes[strings.ToLower(match[2])] = index{
			unique:      match[1] != "",
			table:       strings.ToLower(match[3]),
			firstColumn: strings.ToLower(match[4]),
		}
	}
	for _, name := range requiredUniqueIndexes {
		if implicitUniqueIndexes[name] {
			continue
		}
		if idx, ok := indexes[name]; !ok || !idx.unique {
			t.Errorf("required unique index %s is not a CREATE UNIQUE INDEX in the baseline", name)
		}
	}
	for table, names := range requiredTenantLeadingIndexes {
		for _, name := range names {
			idx, ok := indexes[name]
			switch {
			case !ok:
				t.Errorf("required tenant-leading index %s is not in the baseline", name)
			case idx.table != table:
				t.Errorf("index %s is on %s in the baseline, guard expects %s", name, idx.table, table)
			case idx.firstColumn != "tenant_id":
				t.Errorf("index %s starts with %s in the baseline, want tenant_id", name, idx.firstColumn)
			}
		}
	}

	triggers := map[string]string{}
	for _, match := range baselineTrigger.FindAllStringSubmatch(baseline, -1) {
		triggers[strings.ToLower(match[1])] = strings.ToLower(match[2])
	}
	for table, names := range requiredTriggers {
		for _, name := range names {
			if got, ok := triggers[name]; !ok || got != table {
				t.Errorf("required trigger %s on %s is not created in the baseline (found on %q)", name, table, got)
			}
		}
	}
	for table := range requiredColumns {
		if !slices.Contains(requiredTables, table) {
			t.Errorf("requiredColumns has %s, which is not a required table", table)
		}
	}
}

// TestRLSGuardMatchesBaseline derives tenant-owned tables from requiredColumns
// (every one carries tenant_id) and checks that the guard and the baseline
// enable and force row-level security with the tenant_isolation policy.
func TestRLSGuardMatchesBaseline(t *testing.T) {
	baseline := readBaseline(t)
	for table, columns := range requiredColumns {
		if !slices.Contains(columns, "tenant_id") {
			continue
		}
		if !slices.Contains(requiredRLS, table) {
			t.Errorf("%s has tenant_id but is missing from the RLS guard", table)
			continue
		}
		for _, statement := range []string{
			"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY;",
			"ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY;",
			"CREATE POLICY tenant_isolation ON " + table,
		} {
			if !strings.Contains(baseline, statement) {
				t.Errorf("baseline is missing %q", statement)
			}
		}
	}
	for _, table := range requiredRLS {
		if !slices.Contains(requiredColumns[table], "tenant_id") {
			t.Errorf("RLS guard lists %s, which has no tenant_id column", table)
		}
	}
}

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
		"memberships", "invitations", "sessions", "refresh_tokens",
		"files", "jobs", "audit_events",
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
	// Fresh installs get jobs.trace_parent from the baseline.
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
