package arch_test

import (
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

// authzFragments are the function-name fragments of the permission
// middleware constructors (runtime.FuncForPC names their closures
// ".../authz.Require.func1").
var authzFragments = []string{"/authz.Require.", "/authz.RequireAny."}

// TestEveryProtectedRouteRequiresAPermission fails when a route under
// /api/v1 has no authz.Require/RequireAny in its handler chain and is not an
// explicitly listed public route. It runs over every module in
// internal/bootstrap, so a new module is checked without editing this test.
func TestEveryProtectedRouteRequiresAPermission(t *testing.T) {
	endpoints, _ := moduleRoutes(t)
	seenPublic := map[string]bool{}
	for _, mounted := range endpoints {
		route := mounted.route
		if !strings.HasPrefix(route.Path, "/api/v1/") {
			continue
		}
		key := route.Method + " " + route.Path
		hasAuthz := handlerChainContains(route, authzFragments...)
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
		if prefix == "" || prefix == "/api/v1" {
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
