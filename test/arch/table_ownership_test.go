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
	sqlDistinctFrom = regexp.MustCompile(`(?i)\bis\s+(?:not\s+)?distinct\s+from\b`)
	sqlCTEName      = regexp.MustCompile(`(?i)(?:\bwith|,)\s*(?:recursive\s+)?([a-z_][a-z0-9_]*)\s+as\s*(?:not\s+)?(?:materialized\s+)?\(`)
	// sqlToken splits SQL into quoted identifiers, bare identifiers, and
	// single punctuation characters. It is a tokenizer, not a parser.
	sqlToken = regexp.MustCompile(`"(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$]*|\S`)
)

// sqlKeywordsAfterTableVerb are words that follow FROM/JOIN/INTO/UPDATE but
// are not tables (ON CONFLICT ... DO UPDATE SET, FOR UPDATE SKIP LOCKED,
// IS DISTINCT FROM ...).
var sqlKeywordsAfterTableVerb = map[string]bool{
	"set": true, "skip": true, "nowait": true, "of": true,
	"select": true, "values": true, "excluded": true, "default": true,
	"null": true, "true": true, "false": true,
}

// sqlAliasStop are keywords that may follow a table reference and must not be
// mistaken for its alias.
var sqlAliasStop = map[string]bool{
	"where": true, "join": true, "inner": true, "left": true, "right": true, "full": true,
	"cross": true, "natural": true, "on": true, "using": true, "set": true, "order": true,
	"group": true, "having": true, "limit": true, "offset": true, "returning": true,
	"for": true, "union": true, "intersect": true, "except": true, "window": true,
	"values": true, "select": true, "default": true, "tablesample": true,
}

// sqlTableVerbs introduce a table reference. FROM and USING take a
// comma-separated list (FROM a, b; DELETE ... USING a, b).
var sqlTableVerbs = map[string]bool{"from": true, "join": true, "into": true, "update": true, "using": true}

type sqlTok struct {
	text   string // lower-cased unless quoted; quotes removed
	quoted bool
}

func (tok sqlTok) is(word string) bool { return !tok.quoted && tok.text == word }

// in reports whether tok is an unquoted word from set; a quoted identifier is
// never a keyword.
func (tok sqlTok) in(set map[string]bool) bool { return !tok.quoted && set[tok.text] }

func (tok sqlTok) ident() bool {
	if tok.quoted {
		return true
	}
	c := tok.text[0]
	return c == '_' || (c >= 'a' && c <= 'z')
}

func tokenizeSQL(sql string) []sqlTok {
	raw := sqlToken.FindAllString(sql, -1)
	toks := make([]sqlTok, 0, len(raw))
	for _, text := range raw {
		if strings.HasPrefix(text, `"`) {
			toks = append(toks, sqlTok{text: strings.ReplaceAll(text[1:len(text)-1], `""`, `"`), quoted: true})
			continue
		}
		toks = append(toks, sqlTok{text: strings.ToLower(text)})
	}
	return toks
}

// skipParens returns the index after the balanced group opening at i.
func skipParens(toks []sqlTok, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch {
		case toks[i].is("("):
			depth++
		case toks[i].is(")"):
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return i
}

// sqlTables returns the tables referenced by sql, excluding CTE names,
// function calls, and keywords. It reads every FROM/JOIN/INTO/UPDATE/USING
// target, including comma-separated FROM/USING lists, quoted identifiers, and
// schema-qualified names (reduced to the table name). Subqueries are scanned
// for their own verbs because every token is visited.
func sqlTables(sql string) []string {
	sql = sqlBlockComment.ReplaceAllString(sql, " ")
	sql = sqlLineComment.ReplaceAllString(sql, " ")
	sql = sqlStringLit.ReplaceAllString(sql, "''")
	sql = sqlDistinctFrom.ReplaceAllString(sql, " = ")
	ctes := map[string]bool{}
	for _, match := range sqlCTEName.FindAllStringSubmatch(sql, -1) {
		ctes[strings.ToLower(match[1])] = true
	}
	toks := tokenizeSQL(sql)
	seen := map[string]bool{}
	for i, tok := range toks {
		if !tok.in(sqlTableVerbs) {
			continue
		}
		verb := tok.text
		for j := i + 1; j < len(toks); {
			if toks[j].is("only") || toks[j].is("lateral") {
				j++
				continue
			}
			if toks[j].is("(") {
				if verb == "using" {
					break // JOIN ... USING (col): a column list
				}
				j = skipParens(toks, j) // subquery; its own verbs are visited
			} else {
				if !toks[j].ident() {
					break
				}
				name := toks[j]
				j++
				for j+1 < len(toks) && toks[j].is(".") && toks[j+1].ident() { // schema-qualified
					name = toks[j+1]
					j += 2
				}
				if j < len(toks) && toks[j].is("(") && verb != "into" {
					j = skipParens(toks, j) // FROM unnest(...) is a function call
				} else if !name.in(sqlKeywordsAfterTableVerb) && !name.in(ctes) {
					seen[name.text] = true
				}
			}
			if verb != "from" && verb != "using" {
				break
			}
			// Skip an optional alias and alias column list, then continue
			// only when the reference list goes on with a comma.
			if j < len(toks) && toks[j].is("as") {
				j++
			}
			if j < len(toks) && toks[j].ident() && !toks[j].in(sqlAliasStop) {
				j++
			}
			if j < len(toks) && toks[j].is("(") {
				j = skipParens(toks, j)
			}
			if j >= len(toks) || !toks[j].is(",") {
				break
			}
			j++
		}
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

	// Each case hides a table from a parser that only reads the identifier
	// directly after the verb.
	cases := map[string]struct{ sql, want string }{
		"comma after subquery": {
			sql:  `SELECT * FROM (SELECT id FROM api_keys) b, users u WHERE b.id = u.id`,
			want: "api_keys,users",
		},
		"comma after aliased table": {
			sql:  `SELECT * FROM api_keys AS k, public.users u, "tenants"`,
			want: "api_keys,tenants,users",
		},
		"delete using": {
			sql:  `DELETE FROM api_keys k USING files f, users WHERE k.id = f.id`,
			want: "api_keys,files,users",
		},
		"quoted and schema-qualified": {
			sql:  `UPDATE "public"."tenants" SET name = 'x'; INSERT INTO "users" (id) VALUES (1)`,
			want: "tenants,users",
		},
		"lateral and function in list": {
			sql:  `SELECT * FROM api_keys k, LATERAL unnest(k.scopes) s(scope), LATERAL (SELECT 1 FROM files) f`,
			want: "api_keys,files",
		},
		"join using columns": {
			sql:  `SELECT * FROM api_keys JOIN files USING (id) WHERE true`,
			want: "api_keys,files",
		},
	}
	for name, tc := range cases {
		if got := strings.Join(sqlTables(tc.sql), ","); got != tc.want {
			t.Errorf("%s: sqlTables = %s, want %s", name, got, tc.want)
		}
	}
}
