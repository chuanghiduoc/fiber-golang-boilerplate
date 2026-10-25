package arch_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
)

// tier1Audit names the module whose service records a Tier 1 route's audit
// event in the mutation transaction, and the event's action literal. The
// owner can differ from the module that mounts the route: admin routes call
// identity/files through contracts.
type tier1Audit struct {
	owner  string
	action string
}

// tier1Mutations are audited inside the service's database transaction. They
// must not also carry a route-level Guard intent row.
var tier1Mutations = map[string]tier1Audit{
	"POST /api/v1/auth/register": {"identity", "auth.registered"},
	// Signed-state tenant: UserService records auth.google_login in the
	// sign-in transaction with the state tenant and the signed-in user.
	"GET /api/v1/auth/google/callback": {"identity", "auth.google_login"},
	// Invitations: InvitationService records in the mutation transaction.
	"POST /api/v1/invitations/":                 {"identity", "invitation.created"},
	"DELETE /api/v1/invitations/:id":            {"identity", "invitation.revoked"},
	"POST /api/v1/auth/invitations/accept":      {"identity", "invitation.accepted"},
	"POST /api/v1/auth/invitations/signup":      {"identity", "invitation.accepted"},
	"PUT /api/v1/users/me":                      {"identity", "user.profile_updated"},
	"PUT /api/v1/users/me/password":             {"identity", "user.password_changed"},
	"PUT /api/v1/users/:id":                     {"identity", "user.profile_updated"},
	"DELETE /api/v1/users/:id":                  {"identity", "user.deleted"},
	"PUT /api/v1/admin/users/:id/role":          {"identity", "admin.user.role_changed"},
	"PUT /api/v1/admin/users/:id/roles":         {"identity", "admin.user.roles_replaced"},
	"POST /api/v1/admin/users/:id/ban":          {"identity", "admin.user.banned"},
	"POST /api/v1/admin/users/:id/unban":        {"identity", "admin.user.unbanned"},
	"POST /api/v1/admin/roles/":                 {"identity", "admin.role.created"},
	"PATCH /api/v1/admin/roles/:id":             {"identity", "admin.role.updated"},
	"DELETE /api/v1/admin/roles/:id":            {"identity", "admin.role.deleted"},
	"POST /api/v1/platform/tenants/":            {"identity", "tenant.created"},
	"PATCH /api/v1/platform/tenants/:id/status": {"identity", "tenant.status_changed"},
	"DELETE /api/v1/admin/files/:id":            {"files", "admin.file.deleted"},
	"POST /api/v1/admin/files/:id/restore":      {"files", "admin.file.restored"},
	// API-key lifecycle: apikeys service records in the mutation transaction.
	"POST /api/v1/admin/api-keys/":           {"apikeys", "api_key.created"},
	"PUT /api/v1/admin/api-keys/:id":         {"apikeys", "api_key.updated"},
	"DELETE /api/v1/admin/api-keys/:id":      {"apikeys", "api_key.revoked"},
	"POST /api/v1/admin/api-keys/:id/revoke": {"apikeys", "api_key.revoked"},
	"POST /api/v1/admin/api-keys/:id/rotate": {"apikeys", "api_key.rotated"},
}

// auditPackage identifies audithttp.Guard by full import path.
const auditPackage = "internal/platform/audit/audithttp"

// tier1Recorders are the in-transaction audit calls a Tier 1 service uses.
var tier1Recorders = []string{"audit.RecordInTx(", ".RecordTx("}

// TestTier1RoutesRecordInTransaction makes the Tier 1 exemption verifiable:
// a route may skip audithttp.Guard only if the owning module has a non-test
// source file that both names the route's action literal and calls
// RecordInTx/RecordTx. This is a source grep, not a call-graph proof: it
// catches a renamed action, a removed in-transaction write, or a wrong owner,
// but not a call that sits in the right file on the wrong code path (the
// owning service's tests cover that).
func TestTier1RoutesRecordInTransaction(t *testing.T) {
	root := repoRoot(t)
	sources := map[string][]string{}
	for key, want := range tier1Mutations {
		files, ok := sources[want.owner]
		if !ok {
			files = moduleSources(t, filepath.Join(root, "internal", "modules", want.owner))
			sources[want.owner] = files
		}
		found := false
		for _, src := range files {
			if strings.Contains(src, `"`+want.action+`"`) && containsAny(src, tier1Recorders) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Tier 1 route %s: no internal/modules/%s source records %q with RecordInTx/RecordTx",
				key, want.owner, want.action)
		}
	}
}

// moduleSources returns the contents of every non-test Go file under dir.
func moduleSources(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
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
		out = append(out, string(raw))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// fallbackAudited are existing mutating routes that are neither Tier 1 nor
// wrapped by audithttp.Guard. They are recorded only by the best-effort global
// audithttp.Middleware (plus, for the receiver, a best-effort service event), so
// an unavailable audit store does not fail them. Privileged mutations must be
// Tier 1 or guarded; only non-privileged writes belong here.
var fallbackAudited = map[string]bool{
	// Public receiver: authenticated by the source HMAC, deduplicated per
	// (source, webhook-id); every accepted event is stored.
	"POST /api/v1/webhooks/inbound/:id": true,
	// The caller's own notification state, not privileged configuration.
	"POST /api/v1/notifications/read-all":   true,
	"POST /api/v1/notifications/:id/read":   true,
	"PUT /api/v1/notification-preferences/": true,
}

func TestEveryMutatingRouteIsTier1OrGuarded(t *testing.T) {
	endpoints, _ := moduleRoutes(t)

	seenTier1 := make(map[string]bool, len(tier1Mutations))
	seenFallback := make(map[string]bool, len(fallbackAudited))
	for _, mounted := range endpoints {
		route := mounted.route
		mutating := route.Method == fiber.MethodPost ||
			route.Method == fiber.MethodPut ||
			route.Method == fiber.MethodPatch ||
			route.Method == fiber.MethodDelete ||
			(route.Method == fiber.MethodGet && strings.HasSuffix(route.Path, "/google/callback"))
		if !mutating {
			continue
		}

		key := route.Method + " " + route.Path
		if _, tier1 := tier1Mutations[key]; tier1 {
			seenTier1[key] = true
			if handlerChainHas(route, auditPackage, "Guard") {
				t.Errorf("Tier 1 route %s must not also carry audithttp.Guard (double intent row)", key)
			}
			continue
		}
		if fallbackAudited[key] {
			seenFallback[key] = true
			continue
		}

		if problem := guardedRouteProblem(route); problem != "" {
			t.Errorf("module %s: mutating route %s %s", mounted.module, key, problem)
		}
	}

	for key := range tier1Mutations {
		if !seenTier1[key] {
			t.Errorf("Tier 1 route %s is missing; update the coverage map with the real route", key)
		}
	}
	for key := range fallbackAudited {
		if !seenFallback[key] {
			t.Errorf("fallback-audited route %s no longer exists (or became guarded); remove it from the list", key)
		}
	}
}

// guardedRouteProblem returns why a mutating route that is neither Tier 1
// nor fallback-audited breaks the audit rule, or "". audithttp.Guard must run
// before the final handler (otherwise it never wraps the mutation) and after
// idempotency when the route has one (so replays are not audited again).
func guardedRouteProblem(route fiber.Route) string {
	guardIndex := handlerIndex(route, auditPackage, "Guard")
	idempotencyIndex := -1
	for i, handler := range route.Handlers {
		if strings.HasSuffix(handlerName(handler), "/test/arch_test.testIdempotency") {
			idempotencyIndex = i
		}
	}
	switch {
	case guardIndex < 0:
		return "is neither Tier 1 nor wrapped by audithttp.Guard"
	case !handlerBeforeFinal(route, auditPackage, "Guard"):
		return "runs audithttp.Guard only after its final handler; move it before the handler"
	case idempotencyIndex > guardIndex:
		return fmt.Sprintf("must run idempotency before audithttp.Guard (idempotency=%d guard=%d)", idempotencyIndex, guardIndex)
	}
	return ""
}

func TestGuardedRouteProblem(t *testing.T) {
	guard := audithttp.Guard(audit.Noop{}, audithttp.Policy{})
	cases := map[string]struct {
		handlers []fiber.Handler
		ok       bool
	}{
		"idempotency then guard":    {handlers: []fiber.Handler{testIdempotency, guard, finalHandler}, ok: true},
		"guard without idempotency": {handlers: []fiber.Handler{guard, finalHandler}, ok: true},
		"no guard":                  {handlers: []fiber.Handler{testIdempotency, finalHandler}},
		"guard after handler":       {handlers: []fiber.Handler{testIdempotency, finalHandler, guard}},
		"guard before idempotency":  {handlers: []fiber.Handler{guard, testIdempotency, finalHandler}},
	}
	for name, tc := range cases {
		route := fiber.Route{Method: fiber.MethodPost, Path: "/api/v1/x", Handlers: tc.handlers}
		if problem := guardedRouteProblem(route); (problem == "") != tc.ok {
			t.Errorf("%s: guardedRouteProblem = %q, want ok=%v", name, problem, tc.ok)
		}
	}
}
