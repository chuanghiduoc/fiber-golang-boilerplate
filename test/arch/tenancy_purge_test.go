package arch_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tablesWithTenantID returns the tables a migration section creates whose
// top-level column list contains tenant_id. It lexes the SQL with the shared
// parser, so quoted names, schema qualifiers, comments, and nested constraint
// parentheses cannot hide the column (or invent it from a comment).
func tablesWithTenantID(sql string) []string {
	toks := lexSQL(sql)
	var out []string
	for i := 0; i < len(toks); i++ {
		if !toks[i].is("create") {
			continue
		}
		j := i + 1
		for j < len(toks) && (toks[j].is("global") || toks[j].is("local") ||
			toks[j].is("temp") || toks[j].is("temporary") || toks[j].is("unlogged")) {
			j++
		}
		if j >= len(toks) || !toks[j].is("table") {
			continue
		}
		j++
		if j+2 < len(toks) && toks[j].is("if") && toks[j+1].is("not") && toks[j+2].is("exists") {
			j += 3
		}
		if j >= len(toks) || !toks[j].ident() {
			continue
		}
		name, next := qualifiedName(toks, j)
		if next >= len(toks) || !toks[next].is("(") {
			continue
		}
		depth := 0
		tenantID := false
		for k := next; k < len(toks); k++ {
			switch {
			case toks[k].is("("):
				depth++
			case toks[k].is(")"):
				depth--
				if depth == 0 {
					if tenantID {
						out = append(out, name.text)
					}
					i = k
					k = len(toks)
				}
			case depth == 1 && toks[k].text == "tenant_id":
				tenantID = true
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestTenantOwnedTablesRegisterPurger fails when a module owns a table with a
// tenant_id column but bootstrap does not register a tenancy.Purger for that
// module. Without the purger, deleting a tenant leaves its rows behind and the
// identity purge job cannot finish. Table owners come from the migrations'
// `-- owner:` sections, so a table created in another module's section is
// caught by TestMigrationsCreateOnlyOwnedTables before it can hide here.
func TestTenantOwnedTablesRegisterPurger(t *testing.T) {
	root := repoRoot(t)
	owners := migrationOwnerSet(t, root)
	modules, _ := productionBootstrap(t)
	registered := map[string]bool{}
	for _, provider := range modules.Tenancy.Providers() {
		registered[provider.Name] = true
	}

	paths, err := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	tenantTables := map[string][]string{}
	// Platform tables have no module owner; a platform purger is registered
	// under its table name (the jobs table's purger is "jobs"). audit_events
	// is append-only and deliberately never purged.
	platformTenantTables := map[string]bool{}
	for _, path := range paths {
		name := filepath.Base(path)
		match := migrationFile.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for sectionOwner, sql := range ownerSections(string(raw), ownerPrefix(match[2], owners)) {
			if sectionOwner == "platform" {
				for _, table := range tablesWithTenantID(sql) {
					platformTenantTables[table] = true
				}
				continue
			}
			if tables := tablesWithTenantID(sql); len(tables) > 0 {
				tenantTables[sectionOwner] = append(tenantTables[sectionOwner], tables...)
			}
		}
	}
	if len(tenantTables) == 0 {
		t.Fatal("no tenant-owned tables found; the migration or parser layout changed")
	}
	for owner, tables := range tenantTables {
		if !registered[owner] {
			slices.Sort(tables)
			t.Errorf("module %q owns tenant tables %s but internal/bootstrap registers no tenancy.Purger for it",
				owner, strings.Join(tables, ", "))
		}
	}
	for provider := range registered {
		if _, ok := tenantTables[provider]; !ok && !platformTenantTables[provider] {
			t.Errorf("tenancy provider %q has no tenant-owned table; remove the registration", provider)
		}
	}
}

func TestTablesWithTenantID(t *testing.T) {
	cases := map[string]struct {
		sql  string
		want []string
	}{
		"tenant column": {
			sql:  "CREATE TABLE invoices (id BIGINT, tenant_id BIGINT NOT NULL);",
			want: []string{"invoices"},
		},
		"quoted and qualified": {
			sql:  `CREATE TABLE IF NOT EXISTS "public"."invoices" ("tenant_id" BIGINT);`,
			want: []string{"invoices"},
		},
		"no tenant column": {
			sql: "CREATE TABLE feature_flags (id BIGINT, enabled BOOLEAN);",
		},
		"tenant column only in a nested check": {
			sql: "CREATE TABLE files (id BIGINT, CHECK (id > 0));",
		},
		"tenant column in a comment only": {
			sql: "-- tenant_id is not here\nCREATE TABLE jobs (id BIGINT);",
		},
		"multiple tables": {
			sql:  "CREATE TABLE a (tenant_id BIGINT);\nCREATE TABLE b (id BIGINT);\nCREATE TABLE c (tenant_id BIGINT);",
			want: []string{"a", "c"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tablesWithTenantID(tc.sql); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("tablesWithTenantID = %v, want %v", got, tc.want)
			}
		})
	}
}
