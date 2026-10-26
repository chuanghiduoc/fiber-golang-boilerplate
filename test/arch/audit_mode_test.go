package arch_test

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// bestEffortGuards lists the reviewed audithttp.Guard sites that may run in
// ModeBestEffort. Privileged mutations are fail-closed (ModeRequired or an
// omitted Mode): an audit-store outage must reject them. No site qualifies
// today, so the map is empty; adding one is a product decision with a reason.
var bestEffortGuards = map[string]string{}

// TestBestEffortAuditGuardsAreAllowlisted reads every audithttp.Guard policy
// in module code and rejects ModeBestEffort unless the site is listed above.
// The route-level audit test cannot see the mode through the returned closure,
// so it inspects the Policy literal (and the folded constant) instead.
func TestBestEffortAuditGuardsAreAllowlisted(t *testing.T) {
	root := repoRoot(t)
	pkgs := typedPackages(t)

	used := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			position := pkg.Fset.Position(file.Pos())
			rel, err := filepath.Rel(root, position.Filename)
			if err != nil {
				t.Fatalf("relative path for %s: %v", position.Filename, err)
			}
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "internal/modules/") || strings.HasSuffix(rel, "_test.go") || ast.IsGenerated(file) {
				continue
			}
			assigned := localExpressions(pkg, file)
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn, ok := pkg.TypesInfo.Uses[selector.Sel].(*types.Func)
				if !ok || fn.Pkg() == nil ||
					fn.Pkg().Path() != modulePath+"/internal/platform/audit/audithttp" || fn.Name() != "Guard" {
					return true
				}
				policy, ok := policyLiteral(pkg, assigned, call.Args[1])
				if !ok {
					return true
				}
				if guardMode(pkg, policy) != "best_effort" {
					return true
				}
				line := pkg.Fset.Position(call.Pos()).Line
				key := rel + ":" + strconv.Itoa(line)
				reason, reviewed := bestEffortGuards[key]
				if !reviewed {
					t.Errorf("%s: audithttp.Guard uses ModeBestEffort; privileged mutations must be fail-closed or listed in bestEffortGuards with a reason", key)
					return true
				}
				if strings.TrimSpace(reason) == "" {
					t.Errorf("bestEffortGuards[%q] has no reason", key)
				}
				used[key] = true
				return true
			})
		}
	}
	for key := range bestEffortGuards {
		if !used[key] {
			t.Errorf("bestEffortGuards[%q] no longer matches a best-effort Guard; remove it", key)
		}
	}
}

// policyLiteral resolves the audithttp.Policy literal passed to Guard, following
// one function-local variable so hoisting the literal cannot hide a mode.
func policyLiteral(pkg *packages.Package, assigned map[types.Object]ast.Expr, expr ast.Expr) (*ast.CompositeLit, bool) {
	if literal, ok := expr.(*ast.CompositeLit); ok {
		return literal, true
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return nil, false
	}
	obj := pkg.TypesInfo.Uses[ident]
	if obj == nil {
		return nil, false
	}
	literal, ok := assigned[obj].(*ast.CompositeLit)
	return literal, ok
}

// guardMode returns the effective audithttp mode of a Policy literal: an
// omitted or empty Mode is required, and only the exact ModeBestEffort value
// is best effort (matching Policy.effectiveMode).
func guardMode(pkg *packages.Package, policy *ast.CompositeLit) string {
	for _, element := range policy.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := field.Key.(*ast.Ident); !ok || ident.Name != "Mode" {
			continue
		}
		if value, ok := stringConstant(pkg, field.Value); ok {
			return value
		}
	}
	return ""
}
