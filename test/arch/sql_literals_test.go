package arch_test

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dynamicSQLAllowlist contains the only non-test query sites that may pass a
// runtime-built SQL string. Every key names the file, enclosing function,
// method, and call ordinal, with the reason beside it. The list is reviewed and
// may only shrink: an entry that no longer matches a call fails the test, and a
// new dynamic call fails until it is converted to a constant or the site is
// deliberately added here and maxDynamicSQLSites is raised.
var dynamicSQLAllowlist = map[string]string{
	"cmd/migrate/main.go:scanRows:Query:1":                        "shared schema-verification runner; each caller passes the literal query it verifies",
	"pkg/database/scope.go:begin:Queue:1":                         "Scoped wraps arbitrary sqlc query constants and cannot make the caller's SQL constant",
	"pkg/database/scope.go:Scan:Query:1":                          "scopedRow defers the caller's constant query until Scan so the RLS transaction wraps it",
	"internal/testutil/pgtest/database.go:setupExternalDB:Exec:1": "throwaway per-test schema name is generated at runtime; never used against application data",
	"internal/testutil/pgtest/database.go:setupExternalDB:Exec:2": "throwaway per-test schema name is generated at runtime; never used against application data",
	"internal/testutil/pgtest/postgres.go:OpenAppPool:Exec:1":     "grants a generated test schema identifier to the test app role",
	"internal/testutil/pgtest/postgres.go:ensureAppRole:Exec:1":   "test fixture creates the fixed app role; the identifier is sanitized with pgx.Identifier",
	"internal/testutil/pgtest/postgres.go:ensureAppRole:Exec:2":   "test fixture configures the fixed app role; the identifier is sanitized with pgx.Identifier",
}

// maxDynamicSQLSites is the reviewed size of dynamicSQLAllowlist. The list may
// only shrink, so it must match this number exactly: adding a new dynamic query
// needs a raised ceiling, and fixing one lowers the list and this number
// together. Either edit is deliberate and reviewable.
const maxDynamicSQLSites = 8

// TestSQLCallsUseConstantStrings rejects a query whose SQL argument is not a
// compile-time constant. It type-checks the non-test packages under internal,
// pkg, and cmd, finds Exec/Query/QueryRow on DBTX-shaped values and Queue on a
// pgx.Batch, and checks the SQL argument with go/types. Generated sqlc files
// and tests are out of scope.
func TestSQLCallsUseConstantStrings(t *testing.T) {
	if len(dynamicSQLAllowlist) != maxDynamicSQLSites {
		t.Fatalf("dynamicSQLAllowlist has %d entries, want exactly %d; the list may only shrink, so lower maxDynamicSQLSites when you remove a fixed site",
			len(dynamicSQLAllowlist), maxDynamicSQLSites)
	}
	root := repoRoot(t)
	pkgs := typedPackages(t)

	used := make(map[string]bool, len(dynamicSQLAllowlist))
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			path := pkg.Fset.Position(file.Pos()).Filename
			rel, err := filepath.Rel(root, path)
			if err != nil {
				t.Fatalf("relative path for %s: %v", path, err)
			}
			rel = filepath.ToSlash(rel)
			if skipSQLScanFile(rel, file) {
				continue
			}
			funcs := fileFuncSpans(file)
			sites := map[string]int{}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				selection := pkg.TypesInfo.Selections[selector]
				if selection == nil {
					return true
				}
				method, ok := selection.Obj().(*types.Func)
				if !ok {
					return true
				}
				argIndex, ok := sqlArgumentIndex(method)
				if !ok || argIndex >= len(call.Args) {
					return true
				}
				arg := call.Args[argIndex]
				if value := pkg.TypesInfo.Types[arg].Value; value != nil && value.Kind() == constant.String {
					return true
				}
				fn := enclosingFunc(funcs, call.Pos())
				if fn == "" {
					fn = "<top-level>"
				}
				site := fn + ":" + method.Name()
				sites[site]++
				key := rel + ":" + site + ":" + strconv.Itoa(sites[site])
				reason, reviewed := dynamicSQLAllowlist[key]
				if !reviewed {
					t.Errorf("%s: %s passes runtime SQL %s (key %q); use a compile-time constant or add a reviewed reason to dynamicSQLAllowlist",
						pkg.Fset.Position(call.Pos()), method.Name(), types.ExprString(arg), key)
					return true
				}
				if strings.TrimSpace(reason) == "" {
					t.Errorf("dynamicSQLAllowlist[%q] has no reason", key)
				}
				used[key] = true
				return true
			})
		}
	}
	for key := range dynamicSQLAllowlist {
		if !used[key] {
			t.Errorf("dynamicSQLAllowlist[%q] no longer matches a dynamic SQL call; remove it", key)
		}
	}
}

func skipSQLScanFile(rel string, file *ast.File) bool {
	if strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, "/sqlc/") || strings.Contains(rel, "/testdata/") {
		return true
	}
	return ast.IsGenerated(file)
}

// sqlArgumentIndex reports which call argument carries SQL when method is a
// DBTX-shaped Exec/Query/QueryRow or pgx.Batch.Queue.
func sqlArgumentIndex(method *types.Func) (int, bool) {
	signature, ok := method.Type().(*types.Signature)
	if !ok {
		return 0, false
	}
	switch method.Name() {
	case "Exec", "Query", "QueryRow":
		params := signature.Params()
		if params.Len() >= 2 && isContextType(params.At(0).Type()) && isStringType(params.At(1).Type()) {
			return 1, true
		}
	case "Queue":
		if receiver := signature.Recv(); receiver != nil && isPGXBatch(receiver.Type()) {
			return 0, true
		}
	}
	return 0, false
}

func isContextType(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "context" && obj.Name() == "Context"
}

func isStringType(t types.Type) bool {
	basic, ok := types.Unalias(t).(*types.Basic)
	return ok && basic.Kind() == types.String
}

func isPGXBatch(t types.Type) bool {
	if pointer, ok := types.Unalias(t).(*types.Pointer); ok {
		t = pointer.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "github.com/jackc/pgx/v5" && obj.Name() == "Batch"
}

type funcSpan struct {
	name       string
	start, end token.Pos
}

func fileFuncSpans(file *ast.File) []funcSpan {
	var spans []funcSpan
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		spans = append(spans, funcSpan{name: fn.Name.Name, start: fn.Pos(), end: fn.End()})
	}
	return spans
}

func enclosingFunc(spans []funcSpan, pos token.Pos) string {
	name := ""
	width := token.NoPos
	for _, span := range spans {
		if pos < span.start || pos >= span.end {
			continue
		}
		if width == token.NoPos || span.end-span.start < width {
			name, width = span.name, span.end-span.start
		}
	}
	return name
}
