package arch_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestTier1OperationsRecordInTransaction verifies the metadata claim: a
// registered operation with Audit.Tier1Action must name an action literal that
// the owning module records through RecordTx/RecordInTx in the mutation
// transaction. The old route-keyed map is gone; this walks the huma metadata
// instead. Before module conversion there are no real Tier 1 operations, so the
// test skips (test/arch's stub paths are prefixed /api/v1/arch-stub/).
func TestTier1OperationsRecordInTransaction(t *testing.T) {
	ops := archOperations(t)
	root := repoRoot(t)
	funcsByOwner := map[string][]auditFunc{}
	checked := 0
	for _, op := range ops {
		if op.Policy.Audit.Tier1Action == "" || strings.HasPrefix(op.Op.Path, "/api/v1/arch-stub/") {
			continue
		}
		checked++
		owner := ownerFromPath(op.Op.Path)
		funcs, ok := funcsByOwner[owner]
		if !ok {
			dir := filepath.Join(root, "internal", "modules", owner)
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("%s %s: unknown Tier 1 owner %q", op.Op.Method, op.Op.Path, owner)
				continue
			}
			funcs = moduleAuditFuncs(t, dir)
			funcsByOwner[owner] = funcs
		}
		if problem := auditLinkProblem(funcs, op.Policy.Audit.Tier1Action); problem != "" {
			t.Errorf("%s %s: internal/modules/%s does not record %q in a transaction: %s",
				op.Op.Method, op.Op.Path, owner, op.Policy.Audit.Tier1Action, problem)
		}
	}
	if checked == 0 {
		t.Skip("no converted module declares a Tier 1 operation yet; arch stub mode")
	}
}

// ownerFromPath maps the canonical route map to the module that owns the
// service writing the Tier 1 action. Routes shared by several modules are
// listed explicitly rather than guessed from an operation ID.
func ownerFromPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/v1/files"):
		return "files"
	case strings.HasPrefix(path, "/api/v1/webhooks/"):
		return "webhooks"
	case strings.HasPrefix(path, "/api/v1/me/api-keys"),
		strings.Contains(path, "/api-keys"):
		return "apikeys"
	case strings.HasPrefix(path, "/api/v1/me/notifications"),
		strings.HasPrefix(path, "/api/v1/me/notification-preferences"),
		strings.Contains(path, "/notifications"):
		return "notifications"
	case strings.HasPrefix(path, "/api/v1/platform/feature-flags"):
		return "features"
	case path == "/api/v1/stats":
		return "admin"
	default:
		return "identity"
	}
}
func moduleAuditFuncs(t *testing.T, dir string) []auditFunc {
	t.Helper()
	fset := token.NewFileSet()
	var sources []moduleSource
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sources = append(sources, moduleSource{name: filepath.ToSlash(rel), src: raw})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return parseAuditFuncs(t, fset, sources)
}

// moduleSource is one non-test module file, named relative to the module root.
type moduleSource struct {
	name string
	src  []byte
}

// auditFunc is one declared function of a module package, reduced to the facts
// the audit-link check needs: the action literals it contains, whether it calls
// the in-transaction recorder, and the local function and method names it calls.
// Function literals are part of their enclosing declaration, so a callback that
// names the action belongs to the mutation that passes it.
type auditFunc struct {
	name      string
	pkg       string // directory relative to the module root
	file      string
	actions   map[string]token.Position
	records   bool
	calls     map[string]bool
	startLine int
}

// parseAuditFuncs parses sources and indexes the functions they declare.
func parseAuditFuncs(t *testing.T, fset *token.FileSet, sources []moduleSource) []auditFunc {
	t.Helper()
	sort.Slice(sources, func(i, j int) bool { return sources[i].name < sources[j].name })
	var out []auditFunc
	for _, source := range sources {
		file, err := parser.ParseFile(fset, source.name, source.src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", source.name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			info := auditFunc{
				name:    fn.Name.Name,
				pkg:     filepath.ToSlash(filepath.Dir(source.name)),
				file:    source.name,
				actions: map[string]token.Position{},
				calls:   map[string]bool{},
			}
			info.startLine = fset.Position(fn.Pos()).Line
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return true
					}
					value, err := strconv.Unquote(n.Value)
					if err == nil {
						info.actions[value] = fset.Position(n.Pos())
					}
				case *ast.CallExpr:
					callee := ""
					switch fun := n.Fun.(type) {
					case *ast.Ident:
						callee = fun.Name
					case *ast.SelectorExpr:
						callee = fun.Sel.Name
					}
					if callee == "RecordTx" || callee == "RecordInTx" {
						info.records = true
					}
					if callee != "" {
						info.calls[callee] = true
					}
				}
				return true
			})
			out = append(out, info)
		}
	}
	return out
}

// auditLinkProblem returns why action is not tied to an in-transaction record,
// or "" when it is. The function holding the action and the recorder function
// must be in the same package and reachable from one another through local
// calls; sharing a file or a callee does not connect them.
func auditLinkProblem(funcs []auditFunc, action string) string {
	var withAction, withRecorder []auditFunc
	for _, fn := range funcs {
		if _, ok := fn.actions[action]; ok {
			withAction = append(withAction, fn)
		}
		if fn.records {
			withRecorder = append(withRecorder, fn)
		}
	}
	switch {
	case len(withAction) == 0:
		return "the action literal is not in any function"
	case len(withRecorder) == 0:
		return "no function calls RecordInTx/RecordTx"
	}
	for _, a := range withAction {
		for _, r := range withRecorder {
			if a.pkg != r.pkg {
				continue
			}
			if a.file == r.file && a.name == r.name && a.startLine == r.startLine {
				return ""
			}
			if samePackageCallPath(funcs, a, r) || samePackageCallPath(funcs, r, a) {
				return ""
			}
		}
	}
	return fmt.Sprintf("action in %s; RecordTx/RecordInTx in %s; record the event in the same function or a called helper",
		auditLocations(withAction), auditLocations(withRecorder))
}

func auditLocations(funcs []auditFunc) string {
	out := make([]string, 0, len(funcs))
	for _, fn := range funcs {
		out = append(out, fmt.Sprintf("%s:%d %s", fn.file, fn.startLine, fn.name))
	}
	return strings.Join(out, ", ")
}

// samePackageCallPath reports whether from reaches to (or is to) through calls
// to functions declared in their shared package. Only names declared in that
// package are followed, so imports and interface methods never bridge two
// unrelated mutation functions.
func samePackageCallPath(funcs []auditFunc, from, to auditFunc) bool {
	byName := map[string][]auditFunc{}
	for _, fn := range funcs {
		if fn.pkg == from.pkg {
			byName[fn.name] = append(byName[fn.name], fn)
		}
	}
	seen := map[string]bool{from.name: true}
	queue := []string{from.name}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, fn := range byName[name] {
			for callee := range fn.calls {
				if callee == to.name {
					return true
				}
				if seen[callee] {
					continue
				}
				if _, ok := byName[callee]; !ok {
					continue
				}
				seen[callee] = true
				queue = append(queue, callee)
			}
		}
	}
	return false
}
