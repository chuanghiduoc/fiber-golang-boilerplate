package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tableOwners maps every table referenced by module or platform SQL to its
// one owner: a module name, or "platform/<package>" for a platform table. A
// table that is not listed is owned by the module whose name it equals or
// prefixes with "_" (orders, orders_items -> orders), so a new module that
// follows the naming convention needs no edit here; anything else must be
// added explicitly.
var tableOwners = map[string]string{
	"tenants":                        "identity",
	"tenant_settings":                "identity",
	"users":                          "identity",
	"user_passwords":                 "identity",
	"user_identities":                "identity",
	"memberships":                    "identity",
	"permissions":                    "identity",
	"roles":                          "identity",
	"role_permissions":               "identity",
	"membership_roles":               "identity",
	"platform_role_assignments":      "identity",
	"invitations":                    "identity",
	"sessions":                       "identity",
	"refresh_tokens":                 "identity",
	"password_reset_tokens":          "identity",
	"email_verification_tokens":      "identity",
	"files":                          "files",
	"feature_flags":                  "features",
	"feature_flag_targets":           "features",
	"notifications":                  "notifications",
	"notification_preferences":       "notifications",
	"webhook_endpoints":              "webhooks",
	"webhook_endpoint_subscriptions": "webhooks",
	"webhook_endpoint_secrets":       "webhooks",
	"webhook_events":                 "webhooks",
	"webhook_deliveries":             "webhooks",
	"webhook_delivery_attempts":      "webhooks",
	"webhook_inbound_sources":        "webhooks",
	"webhook_inbound_events":         "webhooks",
	"api_keys":                       "apikeys",
	"api_key_scopes":                 "apikeys",
	// Platform tables are accessed only by their platform package, never by
	// module SQL (modules enqueue through jobs, audit through audit, ...).
	"jobs":             "platform/jobs",
	"audit_events":     "platform/audit",
	"scheduler_leases": "platform/scheduler",
	"scheduler_runs":   "platform/scheduler",
}

// crossModuleReads are deliberate, reviewed exceptions: "<owner>:<table>",
// where <owner> is the module or platform/<package> whose SQL references a
// table it does not own. Modules have none: they read users/tenants through
// internal/contracts/identity, never through SQL.
var crossModuleReads = map[string]string{
	// The flag evaluator is a platform dependency of every module, so it
	// cannot call the features module; it loads its snapshot with a
	// read-only SELECT of the tables the features module administers.
	"platform/features:feature_flags":        "flag snapshot load (read-only)",
	"platform/features:feature_flag_targets": "flag snapshot load (read-only)",
}

// sqlNotTables are words that follow FROM/JOIN/INTO/UPDATE but are not tables
// (ON CONFLICT ... DO UPDATE SET, FOR UPDATE SKIP LOCKED / OF, ...).
var sqlNotTables = map[string]bool{
	"set": true, "skip": true, "nowait": true, "of": true,
	"select": true, "values": true, "excluded": true, "default": true,
	"null": true, "true": true, "false": true,
}

// sqlClauseEnd are keywords that end a FROM/USING/JOIN list (and a WITH
// list) at the current parenthesis depth, so a later comma is not a table
// separator.
var sqlClauseEnd = map[string]bool{
	"where": true, "group": true, "order": true, "having": true, "limit": true,
	"offset": true, "fetch": true, "returning": true, "for": true, "union": true,
	"intersect": true, "except": true, "window": true, "set": true,
	"select": true, "insert": true, "update": true, "delete": true, "values": true,
	"merge": true,
}

// sqlFromInFunc are functions whose argument syntax uses FROM for something
// other than a table: EXTRACT(EPOCH FROM ts), SUBSTRING(s FROM 2), ...
var sqlFromInFunc = map[string]bool{"extract": true, "substring": true, "trim": true, "overlay": true}

// sqlSubqueryStart are tokens that, directly after "(", start a subquery
// rather than a parenthesized join.
var sqlSubqueryStart = map[string]bool{"select": true, "with": true, "values": true, "table": true}

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
	return c == '_' || (c >= 'a' && c <= 'z') || c >= 0x80
}

// sqlDollarTag matches the opening delimiter of a dollar-quoted string ($$ or
// $tag$); $1 is a parameter, not a tag.
var sqlDollarTag = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*)?\$`)

// lexSQL splits sql into bare words (lower-cased), quoted identifiers, and
// single punctuation characters. Comments are dropped. String literals
// (standard, E'...' with backslash escapes, dollar-quoted), numbers, and $N
// parameters each become one opaque token, so nothing inside a literal can
// look like a table reference. It is a lexer, not a parser.
func lexSQL(sql string) []sqlTok {
	var toks []sqlTok
	isWord := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				return toks
			}
			i += end
		case strings.HasPrefix(sql[i:], "/*"):
			i = skipBlockComment(sql, i)
		case c == '\'':
			i = skipStringLiteral(sql, i, false)
			toks = append(toks, sqlTok{text: "'"})
		case c == '"':
			var name strings.Builder
			j := i + 1
			for ; j < len(sql); j++ {
				if sql[j] == '"' {
					if j+1 < len(sql) && sql[j+1] == '"' {
						name.WriteByte('"')
						j++
						continue
					}
					break
				}
				name.WriteByte(sql[j])
			}
			toks = append(toks, sqlTok{text: name.String(), quoted: true})
			i = j + 1
		case c == '$':
			if tag := sqlDollarTag.FindString(sql[i:]); tag != "" {
				end := strings.Index(sql[i+len(tag):], tag)
				if end < 0 {
					return append(toks, sqlTok{text: "'"})
				}
				i += len(tag) + end + len(tag)
				toks = append(toks, sqlTok{text: "'"})
				continue
			}
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			toks = append(toks, sqlTok{text: sql[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(sql) && (isWord(sql[j]) || sql[j] == '.') {
				j++
			}
			toks = append(toks, sqlTok{text: "0"})
			i = j
		case isWord(c):
			j := i + 1
			for j < len(sql) && isWord(sql[j]) {
				j++
			}
			word := strings.ToLower(sql[i:j])
			if word == "e" && j < len(sql) && sql[j] == '\'' { // E'...' escape string
				i = skipStringLiteral(sql, j, true)
				toks = append(toks, sqlTok{text: "'"})
				continue
			}
			toks = append(toks, sqlTok{text: word})
			i = j
		default:
			toks = append(toks, sqlTok{text: string(c)})
			i++
		}
	}
	return toks
}

// skipBlockComment returns the index after the (possibly nested, as in
// PostgreSQL) block comment starting at i.
func skipBlockComment(sql string, i int) int {
	depth := 0
	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return i
}

// skipStringLiteral returns the index after the quoted literal whose opening
// quote is at i. A doubled quote is an escaped quote; with backslash (E'...') a
// backslash escapes the next byte. Standard strings treat backslash as a
// plain character (standard_conforming_strings).
func skipStringLiteral(sql string, i int, backslash bool) int {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslash && sql[j] == '\\':
			j++
		case sql[j] == '\'':
			if j+1 < len(sql) && sql[j+1] == '\'' {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(sql)
}

// sqlFrame is the scanner state of one parenthesis (or bracket) depth.
type sqlFrame struct {
	opener    string // word before the "(" (extract, unnest, ...)
	fromList  bool   // inside FROM/USING/JOIN: "," starts a table reference
	withList  bool   // inside WITH a AS (...), b AS (...): "," may start a CTE
	recursive bool   // the WITH list is WITH RECURSIVE
	cte       string // this frame is the body of CTE cte (visible once closed)
}

// sqlTables returns the tables sql references and the CTE names it defines.
// It reads every FROM/JOIN/USING/INTO/UPDATE/TABLE/TRUNCATE target, every
// comma-separated FROM/USING list (also after a JOIN ... ON clause, after
// TABLESAMPLE, and inside a parenthesized join), quoted identifiers, and
// schema-qualified names (reduced to the table name). Subqueries are scanned
// for their own verbs because every token is visited. An unqualified
// reference is a CTE only when that CTE was defined earlier in the same
// statement; a non-recursive CTE is visible only after its body.
func sqlTables(sql string) (tables, ctes []string) {
	toks := lexSQL(sql)
	seenTables, seenCTEs := map[string]bool{}, map[string]bool{}
	stmtCTEs := map[string]bool{}
	frames := []sqlFrame{{}}
	expect := ""  // verb whose table reference starts at the next token
	cteBody := -1 // index of the "(" opening the CTE body found by lookahead
	cteName := ""
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		top := &frames[len(frames)-1]
		if verb := expect; verb != "" {
			expect = ""
			listVerb := verb == "from" || verb == "join" || verb == "using"
			switch {
			case tok.is("only") || tok.is("lateral") || tok.is("table"):
				expect = verb
				continue
			case tok.is("(") && listVerb && (i+1 >= len(toks) || !toks[i+1].in(sqlSubqueryStart)):
				// FROM (a JOIN b ON ...): a table list inside parentheses.
				frames = append(frames, sqlFrame{fromList: true})
				expect = verb
				continue
			case tok.ident() && !tok.in(sqlNotTables):
				name, next := qualifiedName(toks, i)
				if listVerb && next < len(toks) && toks[next].is("(") {
					break // FROM unnest(...): a function call; "(" is handled below
				}
				qualified := next > i+1
				i = next - 1
				if !qualified && stmtCTEs[name.text] {
					continue // an unqualified reference to a CTE of this statement
				}
				seenTables[name.text] = true
				continue
			}
		}
		if tok.in(sqlClauseEnd) {
			top.fromList, top.withList = false, false
		}
		switch {
		case tok.is("(") || tok.is("["):
			frame := sqlFrame{}
			if i > 0 && !toks[i-1].quoted {
				frame.opener = toks[i-1].text
			}
			if i == cteBody {
				frame.cte = cteName
			}
			frames = append(frames, frame)
		case tok.is(")") || tok.is("]"):
			if len(frames) > 1 {
				if closed := frames[len(frames)-1]; closed.cte != "" {
					stmtCTEs[closed.cte] = true
				}
				frames = frames[:len(frames)-1]
			}
		case tok.is(";"):
			frames = []sqlFrame{{}}
			stmtCTEs = map[string]bool{}
		case tok.is("with") || (tok.is(",") && top.withList):
			recursive := top.withList && top.recursive
			j := i + 1
			if tok.is("with") && j < len(toks) && toks[j].is("recursive") {
				recursive = true
				j++
			}
			if name, body, ok := cteAt(toks, j); ok {
				top.withList, top.recursive = true, recursive
				seenCTEs[name] = true
				if recursive {
					stmtCTEs[name] = true
				} else {
					cteBody, cteName = body, name
				}
			}
		case tok.is(",") && top.fromList:
			expect = "from"
		case tok.is("from"):
			if (i > 0 && toks[i-1].is("distinct")) || sqlFromInFunc[top.opener] {
				continue // IS DISTINCT FROM x, EXTRACT(EPOCH FROM ts)
			}
			top.fromList = true
			expect = "from"
		case tok.is("join") || tok.is("truncate"):
			top.fromList = true
			expect = "join"
		case tok.is("using"):
			if i+1 < len(toks) && toks[i+1].is("(") {
				continue // JOIN ... USING (col): a column list
			}
			top.fromList = true
			expect = "using"
		case tok.is("into") || tok.is("update") || tok.is("table"):
			expect = tok.text
		}
	}
	return sortedKeys(seenTables), sortedKeys(seenCTEs)
}

// cteAt matches `name [(cols)] AS [NOT] [MATERIALIZED] (` at toks[i] and
// returns the CTE name and the index of its body's "(".
func cteAt(toks []sqlTok, i int) (name string, body int, ok bool) {
	if i >= len(toks) || !toks[i].ident() {
		return "", 0, false
	}
	name = toks[i].text
	j := i + 1
	if j < len(toks) && toks[j].is("(") {
		j = skipParens(toks, j)
	}
	if j >= len(toks) || !toks[j].is("as") {
		return "", 0, false
	}
	j++
	if j < len(toks) && toks[j].is("not") {
		j++
	}
	if j < len(toks) && toks[j].is("materialized") {
		j++
	}
	if j >= len(toks) || !toks[j].is("(") {
		return "", 0, false
	}
	return name, j, true
}

// qualifiedName reads `a`, `a.b`, or `a.b.c` at toks[i] and returns the last
// part and the index after the name.
func qualifiedName(toks []sqlTok, i int) (name sqlTok, next int) {
	name, next = toks[i], i+1
	for next+1 < len(toks) && toks[next].is(".") && toks[next+1].ident() {
		name = toks[next+1]
		next += 2
	}
	return name, next
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

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
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

// sqlSource is one piece of SQL and the owner (module or platform/<package>)
// whose code it is.
type sqlSource struct {
	where string // file, or file:line for a Go literal
	owner string
	sql   string
}

// goSQLLiteral recognizes a Go string literal that holds SQL. It matches the
// upper-case keywords this codebase writes SQL with, followed by an
// identifier, so prose such as "read from cache" or an error message is not
// mistaken for a query.
var goSQLLiteral = regexp.MustCompile(
	`\b(?:FROM|JOIN|INSERT\s+INTO|DELETE\s+FROM|MERGE\s+INTO|TRUNCATE|TABLE)\s+["a-z_]` +
		`|\bUPDATE\s+(?:ONLY\s+)?["a-z_][a-z0-9_."]*(?:\s+(?:AS\s+)?[a-z_][a-z0-9_]*)?\s+SET\b`)

// goSQLLiterals returns the SQL-looking string literals in one Go file.
func goSQLLiterals(fset *token.FileSet, file string, src []byte) ([]sqlSource, error) {
	parsed, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []sqlSource
	ast.Inspect(parsed, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil || !goSQLLiteral.MatchString(value) {
			return true
		}
		pos := fset.Position(lit.Pos())
		out = append(out, sqlSource{where: file + ":" + strconv.Itoa(pos.Line), sql: value})
		return true
	})
	return out, nil
}

// codeSQLSources returns the module query files (internal/modules/*/queries)
// and every SQL-looking string literal in non-test, non-generated Go under
// internal/modules (owner: the module) and internal/platform (owner:
// platform/<package>).
func codeSQLSources(t *testing.T, root string) []sqlSource {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "internal", "modules", "*", "queries", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no module query files found; the glob or layout changed")
	}
	var out []sqlSource
	for _, file := range files {
		rel, _ := filepath.Rel(root, file)
		rel = filepath.ToSlash(rel)
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sqlSource{where: rel, owner: strings.Split(rel, "/")[2], sql: string(raw)})
	}

	fset := token.NewFileSet()
	goLiterals := 0
	for _, area := range []string{"modules", "platform"} {
		err := filepath.WalkDir(filepath.Join(root, "internal", area), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "sqlc" || d.Name() == "testdata" { // generated from queries/, or fixtures
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			parts := strings.Split(rel, "/")
			if len(parts) < 4 {
				return nil // a file directly under internal/modules or internal/platform
			}
			owner := parts[2]
			if area == "platform" {
				owner = "platform/" + parts[2]
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			literals, err := goSQLLiterals(fset, rel, src)
			if err != nil {
				return err
			}
			for _, literal := range literals {
				literal.owner = owner
				out = append(out, literal)
			}
			goLiterals += len(literals)
			return nil
		})
		if err != nil {
			t.Fatalf("scan internal/%s: %v", area, err)
		}
	}
	if goLiterals == 0 {
		t.Fatal("no inline SQL found in internal/platform; the Go literal scan is broken")
	}
	return out
}

// TestCodeSQLOnlyTouchesOwnedTables fails when module SQL (query files or
// inline Go literals) or platform SQL references a table owned by someone
// else, a table with no known owner, or defines a CTE named like another
// owner's table (which would hide a reference to that table).
func TestCodeSQLOnlyTouchesOwnedTables(t *testing.T) {
	root := repoRoot(t)
	modules := migrationOwnerSet(t, root)
	usedExceptions := map[string]bool{}
	for _, source := range codeSQLSources(t, root) {
		tables, ctes := sqlTables(source.sql)
		for _, cte := range ctes {
			if owner := tableOwner(cte, modules); owner != "" && owner != source.owner {
				t.Errorf("%s: CTE %q shadows a table owned by %q; rename the CTE", source.where, cte, owner)
			}
		}
		for _, table := range tables {
			owner := tableOwner(table, modules)
			switch owner {
			case source.owner:
			case "":
				t.Errorf("%s: table %q has no owner; add it to tableOwners", source.where, table)
			default:
				exception := source.owner + ":" + table
				if _, ok := crossModuleReads[exception]; ok {
					usedExceptions[exception] = true
					continue
				}
				t.Errorf("%s: %q references table %q owned by %q; use a contract instead",
					source.where, source.owner, table, owner)
			}
		}
	}
	for exception := range crossModuleReads {
		if !usedExceptions[exception] {
			t.Errorf("cross-owner exception %s is no longer used; remove it", exception)
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
	tables, ctes := sqlTables(sql)
	if got, want := strings.Join(tables, ","), "api_keys,files,webhook_deliveries"; got != want {
		t.Fatalf("sqlTables = %s, want %s", got, want)
	}
	if got, want := strings.Join(ctes, ","), "batch,second"; got != want {
		t.Fatalf("sqlTables ctes = %s, want %s", got, want)
	}

	// Each case hides a table from a parser that only reads the identifier
	// directly after the verb, or that mis-lexes literals or CTE scope.
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
		"cte name scoped to its statement": {
			sql:  `WITH users AS (SELECT 1) SELECT * FROM users; SELECT * FROM users`,
			want: "users",
		},
		"cte body reads the real table": {
			sql:  `WITH users AS (SELECT id FROM public.users) SELECT * FROM users`,
			want: "users",
		},
		"non-recursive cte body sees the table, not itself": {
			sql:  `WITH files AS (SELECT id FROM files) SELECT * FROM files`,
			want: "files",
		},
		"recursive cte self-reference": {
			sql:  `WITH RECURSIVE tree AS (SELECT id FROM files UNION ALL SELECT id FROM tree) SELECT * FROM tree`,
			want: "files",
		},
		"parenthesized join": {
			sql:  `SELECT * FROM (users u JOIN files f ON u.id = f.user_id) WHERE true`,
			want: "files,users",
		},
		"comma after join clause": {
			sql:  `SELECT * FROM api_keys k JOIN LATERAL (SELECT 1) x ON true, users u WHERE true`,
			want: "api_keys,users",
		},
		"comma after join on array": {
			sql:  `SELECT * FROM api_keys k JOIN files f ON f.id = ANY(ARRAY[k.a, k.b]), users`,
			want: "api_keys,files,users",
		},
		"comma after tablesample": {
			sql:  `SELECT * FROM api_keys TABLESAMPLE SYSTEM (1), users`,
			want: "api_keys,users",
		},
		"table statement": {
			sql:  `TABLE users`,
			want: "users",
		},
		"escape string with escaped quote": {
			sql:  `SELECT E'\'' AS a FROM users WHERE b = 'x'`,
			want: "users",
		},
		"standard string keeps backslash": {
			sql:  `SELECT 'a\' FROM api_keys JOIN users ON true`,
			want: "api_keys,users",
		},
		"dollar-quoted string": {
			sql:  `SELECT $q$ FROM tenants $q$, $$ JOIN tenants $$ FROM api_keys WHERE id = $1`,
			want: "api_keys",
		},
		"comment marker inside string": {
			sql:  `SELECT '--' FROM api_keys JOIN users ON true`,
			want: "api_keys,users",
		},
		"nested block comment": {
			sql:  `SELECT 1 /* a /* JOIN tenants */ JOIN tenants */ FROM api_keys`,
			want: "api_keys",
		},
		"from inside extract": {
			sql:  `SELECT EXTRACT(EPOCH FROM created_at), SUBSTRING(name FROM 2) FROM api_keys`,
			want: "api_keys",
		},
		"truncate list": {
			sql:  `TRUNCATE TABLE api_keys, users`,
			want: "api_keys,users",
		},
	}
	for name, tc := range cases {
		if tables, _ := sqlTables(tc.sql); strings.Join(tables, ",") != tc.want {
			t.Errorf("%s: sqlTables = %s, want %s", name, strings.Join(tables, ","), tc.want)
		}
	}
}

func TestGoSQLLiterals(t *testing.T) {
	src := []byte("package p\n" +
		"// SELECT * FROM users in a comment is ignored\n" +
		"const q = `SELECT id FROM jobs WHERE id = $1`\n" +
		"var u = \"UPDATE users SET name = $1\"\n" +
		"var notSQL = []string{\"read from cache\", \"failed to update user\", \"select a FROM\"}\n")
	literals, err := goSQLLiterals(token.NewFileSet(), "p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(literals))
	for _, literal := range literals {
		got = append(got, literal.where+" "+literal.sql)
	}
	want := []string{"p.go:3 SELECT id FROM jobs WHERE id = $1", "p.go:4 UPDATE users SET name = $1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("goSQLLiterals = %q, want %q", got, want)
	}
}
