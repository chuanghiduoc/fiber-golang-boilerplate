package arch_test

import (
	"go/ast"
	"go/constant"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

const authzHTTPPath = modulePath + "/internal/platform/authz/authzhttp"

// routeMethods are the Fiber Router methods that register an endpoint.
var routeMethods = map[string]bool{
	"Get": true, "Post": true, "Put": true, "Patch": true, "Delete": true,
	"Head": true, "Options": true, "All": true,
}

// routeAuthz is one registered route reduced to the facts the permission
// policy needs: its authz permissions (read from the constant arguments passed
// to authzhttp.Require/RequireAny) and whether RequireMFA guards it.
type routeAuthz struct {
	where       string
	method      string
	path        string
	permissions []string
	mfa         bool
}

// TestPlatformPermissionRoutesRequireMFA enforces the MFA rule by permission,
// not by path prefix: every route whose authz middleware accepts a platform
// permission must run authzhttp.RequireMFA before its handler. Platform
// permission constants are the only cross-tenant grants (registered with
// Platform: true), so a route that accepts one is a platform route regardless
// of the URL it mounts on.
func TestPlatformPermissionRoutesRequireMFA(t *testing.T) {
	routes := moduleRouteAuthz(t)
	platformRoutes := 0
	for _, route := range routes {
		platform := false
		for _, permission := range route.permissions {
			if strings.HasPrefix(permission, "platform:") {
				platform = true
			}
		}
		if !platform {
			continue
		}
		platformRoutes++
		if !route.mfa {
			t.Errorf("%s: %s %s accepts a platform permission but does not run authzhttp.RequireMFA before its handler",
				route.where, route.method, route.path)
		}
	}
	if platformRoutes == 0 {
		t.Fatal("no route accepts a platform permission; update this test if platform authz moved")
	}
}

// moduleRouteAuthz parses the non-test route registrations under
// internal/modules and resolves the permission constants they pass to
// authzhttp.Require/RequireAny, including through a function-local variable.
func moduleRouteAuthz(t *testing.T) []routeAuthz {
	t.Helper()
	root := repoRoot(t)
	pkgs := typedPackages(t)
	router := fiberRouterType(t, pkgs)

	var out []routeAuthz
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
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !routeMethods[selector.Sel.Name] || len(call.Args) < 2 {
					return true
				}
				receiver := pkg.TypesInfo.Types[selector.X].Type
				if receiver == nil || !types.Implements(receiver, router) {
					return true
				}
				path, ok := stringConstant(pkg, call.Args[0])
				if !ok {
					return true
				}
				permissions, mfa := routeRequirements(pkg, assigned, call.Args[1:], map[types.Object]bool{})
				if mfa || len(permissions) > 0 {
					out = append(out, routeAuthz{
						where:       rel + ":" + strconv.Itoa(pkg.Fset.Position(call.Pos()).Line),
						method:      strings.ToUpper(selector.Sel.Name),
						path:        path,
						permissions: permissions,
						mfa:         mfa,
					})
				}
				return true
			})
		}
	}
	return out
}

// fiberRouterType finds the fiber.Router interface in the loaded packages.
func fiberRouterType(t *testing.T, pkgs []*packages.Package) *types.Interface {
	t.Helper()
	for _, pkg := range pkgs {
		for _, imported := range pkg.Types.Imports() {
			if imported.Path() != "github.com/gofiber/fiber/v3" {
				continue
			}
			obj := imported.Scope().Lookup("Router")
			if obj == nil {
				continue
			}
			iface, ok := obj.Type().Underlying().(*types.Interface)
			if ok {
				return iface
			}
		}
	}
	t.Fatal("fiber.Router not found in the loaded module packages")
	return nil
}

// localExpressions maps each function-local variable to the expression that
// initializes it, so a middleware variable (mfa, flagWrite, read) can be
// resolved to its constructor call.
func localExpressions(pkg *packages.Package, file *ast.File) map[types.Object]ast.Expr {
	assigned := map[types.Object]ast.Expr{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if i >= len(n.Rhs) {
					break
				}
				ident, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				obj := pkg.TypesInfo.Defs[ident]
				if obj == nil {
					obj = pkg.TypesInfo.Uses[ident]
				}
				if obj != nil {
					assigned[obj] = n.Rhs[i]
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if i >= len(n.Values) {
					break
				}
				if obj := pkg.TypesInfo.Defs[name]; obj != nil {
					assigned[obj] = n.Values[i]
				}
			}
		}
		return true
	})
	return assigned
}

// routeRequirements reads the permissions and MFA marker from the middleware
// arguments, following function-local variables one hop at a time.
func routeRequirements(pkg *packages.Package, assigned map[types.Object]ast.Expr, args []ast.Expr, seen map[types.Object]bool) (permissions []string, mfa bool) {
	for _, arg := range args {
		expr := arg
		if ident, ok := arg.(*ast.Ident); ok {
			obj, ok := pkg.TypesInfo.Uses[ident].(*types.Var)
			if !ok || seen[obj] {
				continue
			}
			rhs, ok := assigned[obj]
			if !ok {
				continue
			}
			seen[obj] = true
			expr = rhs
		}
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		fn, ok := pkg.TypesInfo.Uses[selector.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != authzHTTPPath {
			continue
		}
		switch fn.Name() {
		case "Require", "RequireAny":
			for _, permission := range call.Args {
				if value, ok := stringConstant(pkg, permission); ok {
					permissions = append(permissions, value)
				}
			}
		case "RequireMFA":
			mfa = true
		}
	}
	return permissions, mfa
}

// stringConstant evaluates expr to a string constant with go/types.
func stringConstant(pkg *packages.Package, expr ast.Expr) (string, bool) {
	value := pkg.TypesInfo.Types[expr].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
}
