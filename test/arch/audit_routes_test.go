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
	// MFA management and two-step login verification record in the service
	// transaction.
	"POST /api/v1/auth/mfa/totp/enroll":               {"identity", "auth.mfa_enrollment_started"},
	"POST /api/v1/auth/mfa/totp/confirm":              {"identity", "auth.mfa_enabled"},
	"POST /api/v1/auth/mfa/disable":                   {"identity", "auth.mfa_disabled"},
	"POST /api/v1/auth/mfa/recovery-codes/regenerate": {"identity", "auth.mfa_recovery_codes_regenerated"},
	"POST /api/v1/auth/mfa/verify":                    {"identity", "auth.mfa_login"},
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
	"POST /api/v1/me/erasure":                   {"identity", "user.erasure_requested"},
	"POST /api/v1/admin/users/:id/erasure":      {"identity", "admin.user.erasure_requested"},
	"PUT /api/v1/admin/users/:id/role":          {"identity", "admin.user.role_changed"},
	"PUT /api/v1/admin/users/:id/roles":         {"identity", "admin.user.roles_replaced"},
	"POST /api/v1/admin/users/:id/ban":          {"identity", "admin.user.banned"},
	"POST /api/v1/admin/users/:id/unban":        {"identity", "admin.user.unbanned"},
	"POST /api/v1/admin/roles/":                 {"identity", "admin.role.created"},
	"PATCH /api/v1/admin/roles/:id":             {"identity", "admin.role.updated"},
	"DELETE /api/v1/admin/roles/:id":            {"identity", "admin.role.deleted"},
	"POST /api/v1/platform/tenants/":            {"identity", "tenant.created"},
	"PATCH /api/v1/platform/tenants/:id/status": {"identity", "tenant.status_changed"},
	"POST /api/v1/platform/users/:id/mfa/reset": {"identity", "admin.user.mfa_reset"},
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

// TestTier1RoutesRecordInTransaction makes the Tier 1 exemption verifiable:
// a route may skip audithttp.Guard only if the owning module records its action
// in a transaction. The action literal must sit in the same function as a
// RecordInTx/RecordTx call, or the two must be connected by calls in the same
// package. That covers the reviewed shapes without accepting a literal and a
// recorder that merely share a file: a mutation calling an event builder that
// names the action, a mutation naming the action and calling a shared commit
// helper that records it, or a callback helper recording the event the
// mutation returns (apikeys' mutate -> recordAudit).
func TestTier1RoutesRecordInTransaction(t *testing.T) {
	root := repoRoot(t)
	owners := map[string][]auditFunc{}
	for key, want := range tier1Mutations {
		funcs, ok := owners[want.owner]
		if !ok {
			funcs = moduleAuditFuncs(t, filepath.Join(root, "internal", "modules", want.owner))
			owners[want.owner] = funcs
		}
		if problem := auditLinkProblem(funcs, want.action); problem != "" {
			t.Errorf("Tier 1 route %s: internal/modules/%s does not record %q in a transaction: %s",
				key, want.owner, want.action, problem)
		}
	}
}

// moduleAuditFuncs parses every non-test Go file under dir. Both the action
// literal and the recorder call must be in this one module's source tree.
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

// fallbackAudited are existing mutating routes that are neither Tier 1 nor
// wrapped by audithttp.Guard. They are recorded only by the best-effort global
// audithttp.Middleware (plus, for the receiver, a best-effort service event), so
// an unavailable audit store does not fail them. Privileged mutations must be
// Tier 1 or guarded; only non-privileged writes belong here.
var fallbackAudited = map[string]bool{
	// Public receiver: authenticated by the source HMAC, deduplicated per
	// (source, webhook-id); every accepted event is stored.
	"POST /api/v1/webhooks/inbound/:id": true,
	// Redeems the callback's single-use code for tokens the callback already
	// audited as auth.google_login; it changes no privileged state itself.
	"POST /api/v1/auth/google/exchange": true,
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

func TestAuditLinkProblem(t *testing.T) {
	const action = "thing.changed"
	cases := map[string]struct {
		sources    map[string]string
		wantBroken bool
	}{
		"same function": {
			sources: map[string]string{"service/a.go": `package service

func apply(ctx context.Context, tx TxRepos) error {
	return tx.Audit.RecordTx(ctx, audit.Event{Action: "thing.changed"})
}`},
		},
		"event builder called by the recorder": {
			sources: map[string]string{"service/a.go": `package service

func apply(ctx context.Context, tx TxRepos) error {
	return tx.Audit.RecordTx(ctx, event())
}

func event() audit.Event { return audit.Event{Action: "thing.changed"} }`},
		},
		"mutation calls the commit helper": {
			sources: map[string]string{"service/a.go": `package service

func apply(ctx context.Context, tx TxRepos) error {
	return commit(ctx, tx, "thing.changed")
}

func commit(ctx context.Context, tx TxRepos, action string) error {
	return tx.Audit.RecordTx(ctx, audit.Event{Action: action})
}`},
		},
		"callback helper records the returned event": {
			sources: map[string]string{"service/a.go": `package service

func apply(ctx context.Context) error {
	return s.mutate(ctx, func(ctx context.Context) (*audit.Event, error) {
		return &audit.Event{Action: "thing.changed"}, nil
	})
}

func (s *service) mutate(ctx context.Context, fn func(context.Context) (*audit.Event, error)) error {
	return s.recordAudit(ctx, nil)
}

func (s *service) recordAudit(ctx context.Context, event *audit.Event) error {
	return tx.Audit.RecordTx(ctx, *event)
}`},
		},
		"unrelated functions in one file": {
			wantBroken: true,
			sources: map[string]string{"service/a.go": `package service

func apply() string { return "thing.changed" }

func record(ctx context.Context, tx TxRepos) error {
	return tx.Audit.RecordTx(ctx, audit.Event{})
}`},
		},
		"unrelated functions in one package": {
			wantBroken: true,
			sources: map[string]string{
				"service/a.go": `package service

func apply() string { return "thing.changed" }`,
				"service/b.go": `package service

func record(ctx context.Context, tx TxRepos) error {
	return tx.Audit.RecordTx(ctx, audit.Event{})
}`,
			},
		},
		"a common callee does not link them": {
			wantBroken: true,
			sources: map[string]string{"service/a.go": `package service

func apply() {
	helper()
	_ = "thing.changed"
}

func record(ctx context.Context, tx TxRepos) error {
	helper()
	return tx.Audit.RecordTx(ctx, audit.Event{})
}

func helper() {}`},
		},
		"no action literal": {
			wantBroken: true,
			sources: map[string]string{"service/a.go": `package service

func record(ctx context.Context, tx TxRepos) error {
	return tx.Audit.RecordTx(ctx, audit.Event{})
}`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fset := token.NewFileSet()
			sources := make([]moduleSource, 0, len(tc.sources))
			for file, src := range tc.sources {
				sources = append(sources, moduleSource{name: file, src: []byte(src)})
			}
			problem := auditLinkProblem(parseAuditFuncs(t, fset, sources), action)
			if (problem != "") != tc.wantBroken {
				t.Errorf("auditLinkProblem = %q, wantBroken=%v", problem, tc.wantBroken)
			}
		})
	}
}
