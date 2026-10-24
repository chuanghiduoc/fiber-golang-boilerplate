package arch_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// tableOwners maps every table referenced by module SQL to the one module
// that owns it. A table that is not listed is owned by the module whose name
// it equals or prefixes with "_" (orders, orders_items -> orders), so a new
// module that follows the naming convention needs no edit here; anything
// else must be added explicitly.
var tableOwners = map[string]string{
	"tenants":                   "identity",
	"users":                     "identity",
	"refresh_tokens":            "identity",
	"password_reset_tokens":     "identity",
	"email_verification_tokens": "identity",
	"files":                     "files",
	"feature_flags":             "features",
	"feature_flag_targets":      "features",
	"notifications":             "notifications",
	"notification_preferences":  "notifications",
	"webhook_endpoints":         "webhooks",
	"webhook_endpoint_secrets":  "webhooks",
	"webhook_deliveries":        "webhooks",
	"webhook_delivery_attempts": "webhooks",
	"webhook_inbound_sources":   "webhooks",
	"webhook_inbound_events":    "webhooks",
	"api_keys":                  "apikeys",
	// Platform tables (jobs, audit_events, scheduler_*) are accessed only by
	// internal/platform code, never by module queries.
	"jobs":             "platform",
	"audit_events":     "platform",
	"scheduler_leases": "platform",
	"scheduler_runs":   "platform",
}

// crossModuleReads are deliberate, reviewed exceptions: "<module>:<table>".
// There are none today: other modules read users/tenants through
// internal/contracts/identity, never through SQL.
var crossModuleReads = map[string]string{}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlStringLit    = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlTableRef     = regexp.MustCompile(`(?i)\b(from|join|into|update)\s+([a-z_][a-z0-9_]*(?:\.[a-z_][a-z0-9_]*)?)\b\s*(\()?`)
	sqlDistinctFrom = regexp.MustCompile(`(?i)\bis\s+(?:not\s+)?distinct\s+from\b`)
	sqlCTEName      = regexp.MustCompile(`(?i)(?:\bwith|,)\s*(?:recursive\s+)?([a-z_][a-z0-9_]*)\s+as\s*(?:not\s+)?(?:materialized\s+)?\(`)
)

// sqlKeywordsAfterTableVerb are words that follow FROM/JOIN/INTO/UPDATE but
// are not tables (ON CONFLICT ... DO UPDATE SET, FOR UPDATE SKIP LOCKED,
// JOIN LATERAL, IS DISTINCT FROM ...).
var sqlKeywordsAfterTableVerb = map[string]bool{
	"set": true, "skip": true, "nowait": true, "of": true, "lateral": true, "only": true,
	"select": true, "values": true, "excluded": true, "unnest": true, "generate_series": true,
	"null": true, "true": true, "false": true,
}

// sqlTables returns the tables referenced by sql, excluding CTE names,
// function calls, and keywords.
func sqlTables(sql string) []string {
	sql = sqlBlockComment.ReplaceAllString(sql, " ")
	sql = sqlLineComment.ReplaceAllString(sql, " ")
	sql = sqlStringLit.ReplaceAllString(sql, "''")
	sql = sqlDistinctFrom.ReplaceAllString(sql, " = ")
	ctes := map[string]bool{}
	for _, match := range sqlCTEName.FindAllStringSubmatch(sql, -1) {
		ctes[strings.ToLower(match[1])] = true
	}
	seen := map[string]bool{}
	for _, match := range sqlTableRef.FindAllStringSubmatch(sql, -1) {
		verb, name := strings.ToLower(match[1]), strings.ToLower(match[2])
		// FROM unnest(...) is a function call; INTO t (cols) is a table.
		if match[3] == "(" && (verb == "from" || verb == "join") {
			continue
		}
		if i := strings.LastIndexByte(name, '.'); i >= 0 { // schema-qualified
			name = name[i+1:]
		}
		if sqlKeywordsAfterTableVerb[name] || ctes[name] {
			continue
		}
		seen[name] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func tableOwner(table string, modules map[string]bool) string {
	if owner, ok := tableOwners[table]; ok {
		return owner
	}
	best := ""
	for module := range modules {
		if (table == module || strings.HasPrefix(table, module+"_")) && len(module) > len(best) {
			best = module
		}
	}
	return best
}

// TestModuleQueriesOnlyTouchOwnedTables parses internal/modules/*/queries/*.sql
// and fails when a module references a table owned by another module (or a
// platform table), or a table with no known owner.
func TestModuleQueriesOnlyTouchOwnedTables(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "internal", "modules", "*", "queries", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no module query files found; the glob or layout changed")
	}
	modules := migrationOwnerSet(t, root)
	usedExceptions := map[string]bool{}
	for _, file := range files {
		rel, _ := filepath.Rel(root, file)
		module := strings.Split(filepath.ToSlash(rel), "/")[2]
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range sqlTables(string(raw)) {
			owner := tableOwner(table, modules)
			switch owner {
			case module:
			case "":
				t.Errorf("%s: table %q has no owner; add it to tableOwners (or name it %s_*)", rel, table, module)
			default:
				exception := module + ":" + table
				if _, ok := crossModuleReads[exception]; ok {
					usedExceptions[exception] = true
					continue
				}
				t.Errorf("%s: module %q references table %q owned by %q; use a contract instead", rel, module, table, owner)
			}
		}
	}
	for exception := range crossModuleReads {
		if !usedExceptions[exception] {
			t.Errorf("cross-module exception %s is no longer used; remove it", exception)
		}
	}
}

func TestSQLTablesParser(t *testing.T) {
	sql := `
-- name: Claim :many
-- reads from the users table in a comment only
WITH batch AS (
    SELECT id FROM webhook_deliveries WHERE note <> 'from users'
    FOR UPDATE SKIP LOCKED
), second AS (SELECT 1)
UPDATE webhook_deliveries d SET status = 'x' FROM batch JOIN public.files f ON true
WHERE d.id = batch.id AND d.x IS DISTINCT FROM d.y;
INSERT INTO api_keys (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET id = EXCLUDED.id;
SELECT * FROM unnest($1::bigint[]) AS u(id) /* JOIN tenants */;`
	got := strings.Join(sqlTables(sql), ",")
	if want := "api_keys,files,webhook_deliveries"; got != want {
		t.Fatalf("sqlTables = %s, want %s", got, want)
	}
}
