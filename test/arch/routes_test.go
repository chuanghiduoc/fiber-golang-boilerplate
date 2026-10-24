package arch_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// publicRoutes are the deliberate exceptions to "every /api/v1 route checks a
// permission". Each one is authenticated another way or is intentionally
// anonymous, and still carries a rate limit. Adding to this list is a product
// decision; review it like a permission change.
var publicRoutes = map[string]string{
	"POST /api/v1/auth/register":            "anonymous sign-up (feature-flag gated)",
	"POST /api/v1/auth/login":               "credential check",
	"POST /api/v1/auth/refresh":             "refresh-token possession",
	"POST /api/v1/auth/logout":              "refresh-token possession",
	"POST /api/v1/auth/forgot-password":     "anonymous, non-enumerating",
	"POST /api/v1/auth/reset-password":      "reset-token possession",
	"POST /api/v1/auth/verify-email":        "verification-token possession",
	"POST /api/v1/auth/resend-verification": "anonymous, non-enumerating",
	"GET /api/v1/auth/google":               "OAuth redirect",
	"GET /api/v1/auth/google/callback":      "signed OAuth state",
	"POST /api/v1/webhooks/inbound/:id":     "source HMAC signature",
}

// authzPackage and authzFuncs identify the permission middleware
// constructors by full import path (runtime.FuncForPC names their closures
// "<module>/internal/platform/authz.Require.func1").
const authzPackage = "internal/platform/authz"

var authzFuncs = []string{"Require", "RequireAny"}

// TestEveryProtectedRouteRequiresAPermission fails when a route under
// /api/v1 or /api/v2 has no authz.Require/RequireAny in its handler chain and
// is not an explicitly listed public route. It runs over every module in
// internal/bootstrap, so a new module is checked without editing this test.
func TestEveryProtectedRouteRequiresAPermission(t *testing.T) {
	endpoints, _ := moduleRoutes(t)
	seenPublic := map[string]bool{}
	for _, mounted := range endpoints {
		route := mounted.route
		if !underAPIPrefix(route.Path) {
			continue
		}
		key := route.Method + " " + route.Path
		hasAuthz := handlerChainHas(route, authzPackage, authzFuncs...)
		if _, public := publicRoutes[key]; public {
			seenPublic[key] = true
			if hasAuthz {
				t.Errorf("%s is listed as public but checks a permission; remove it from publicRoutes", key)
			}
			continue
		}
		if !hasAuthz {
			t.Errorf("module %s: %s has no authz.Require in its handler chain; protect it or add it to publicRoutes with a reason",
				mounted.module, key)
		}
	}
	for key := range publicRoutes {
		if !seenPublic[key] {
			t.Errorf("public route %s no longer exists; remove it from publicRoutes", key)
		}
	}
}

// TestGroupMiddlewareDoesNotCoverAnotherModule catches the /admin overlap
// bug class: Fiber mounts Group(prefix, middleware...) as Use(prefix), which
// runs for every route below the prefix, including routes another module
// registers there (double auth, double rate limiting, or a stricter policy
// leaking onto someone else's routes). A module that shares a prefix must
// attach its middleware per route instead.
func TestGroupMiddlewareDoesNotCoverAnotherModule(t *testing.T) {
	endpoints, groups := moduleRoutes(t)
	for _, group := range groups {
		prefix := strings.TrimSuffix(group.route.Path, "/")
		if prefix == "" || slices.Contains(apiPrefixes, prefix) {
			t.Errorf("module %s mounts middleware on %q, which covers every module; use per-route middleware",
				group.module, group.route.Path)
			continue
		}
		for _, endpoint := range endpoints {
			if endpoint.module == group.module {
				continue
			}
			path := endpoint.route.Path
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				t.Errorf("module %s group middleware on %s also runs for module %s route %s %s",
					group.module, prefix, endpoint.module, endpoint.route.Method, path)
			}
		}
	}
}

// nonModuleDirs are directories under internal/modules that are deliberately
// not registered in internal/bootstrap. There are none today.
var nonModuleDirs = map[string]string{}

// TestEveryModuleDirectoryIsRegistered fails when internal/modules/<name>
// exists but no module named <name> is in bootstrap.Build's list (its routes,
// catalogs, and jobs would silently never mount), or when a registered
// module's Name() has no directory of that name.
func TestEveryModuleDirectoryIsRegistered(t *testing.T) {
	modules, _ := productionModules(t)
	registered := map[string]bool{}
	for _, m := range modules {
		registered[m.Name()] = true
	}
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal", "modules"))
	if err != nil {
		t.Fatalf("read modules: %v", err)
	}
	dirs := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		dirs[name] = true
		_, exempt := nonModuleDirs[name]
		switch {
		case exempt && registered[name]:
			t.Errorf("internal/modules/%s is registered; remove it from nonModuleDirs", name)
		case !exempt && !registered[name]:
			t.Errorf("internal/modules/%s is not registered in internal/bootstrap (no module Name() == %q)", name, name)
		}
	}
	for name := range registered {
		if !dirs[name] {
			t.Errorf("registered module %q has no internal/modules/%s directory", name, name)
		}
	}
	for name := range nonModuleDirs {
		if !dirs[name] {
			t.Errorf("nonModuleDirs entry %q no longer exists; remove it", name)
		}
	}
}

// TestModuleNamesAreUnique keeps the registry and the catalogs in
// step: RegisterCatalogs owns entries by Module.Name(), so names must be
// unique and stable.
func TestModuleNamesAreUnique(t *testing.T) {
	modules, _ := productionModules(t)
	seen := map[string]bool{}
	for _, m := range modules {
		if seen[m.Name()] {
			t.Errorf("module name %q is used twice", m.Name())
		}
		seen[m.Name()] = true
	}
}
