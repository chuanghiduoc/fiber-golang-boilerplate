package arch_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

// publicOperations is the explicit allowlist for anonymous operations. Adding
// an entry is a product decision; review it like a permission change. The key
// is "METHOD path" because that is what clients see. Entries may remain after a
// module conversion renames a path; stale entries are harmless documentation,
// while a public operation with no entry fails.
var publicOperations = map[string]string{
	"POST /api/v1/auth/register":                     "account self-registration",
	"POST /api/v1/auth/login":                        "credential check",
	"POST /api/v1/auth/refresh":                      "refresh-token possession (cookie or body)",
	"POST /api/v1/auth/logout":                       "refresh-token possession (cookie or body)",
	"POST /api/v1/auth/password-resets":              "non-enumerating reset request",
	"POST /api/v1/auth/password-resets/complete":     "possession of a reset token",
	"POST /api/v1/auth/email-verifications":          "non-enumerating verification request",
	"POST /api/v1/auth/email-verifications/complete": "possession of a verification token",
	"GET /api/v1/auth/google":                        "starts a public OAuth flow",
	"GET /api/v1/auth/google/callback":               "signed OAuth state and PKCE cookie",
	"POST /api/v1/auth/google/exchanges":             "single-use 256-bit exchange code",
	"POST /api/v1/auth/mfa/verifications":            "sealed single-use MFA challenge plus code",
	"POST /api/v1/auth/invitations/signup":           "invitation token possession",
	"POST /api/v1/webhooks/inbound/{sourceId}":       "source HMAC signature",
	"POST /api/v1/auth/passkeys/options":             "public passkey login challenge; the assertion is the credential",
	"POST /api/v1/auth/passkeys/verifications":       "public passkey assertion; the WebAuthn signature is the credential",
	"GET /healthz":                                   "liveness probe",
	"GET /readyz":                                    "readiness probe",
	"GET /health":                                    "readiness probe alias",
	"GET /metrics":                                   "Prometheus scrape, optionally Bearer-token protected",
}

// bestEffortGuards lists reviewed Tier 2 Guards that may run in best-effort
// mode. Privileged mutations are fail-closed; no site qualifies today.
var infrastructurePaths = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/health":  true,
	"/metrics": true,
}

var bestEffortGuards = map[string]string{
	"PATCH /api/v1/me/notifications/{notificationId}":          "caller-owned notification read state",
	"PATCH /api/v1/me/notifications":                           "caller-owned notification read state",
	"PUT /api/v1/me/notification-preferences/{type}/{channel}": "caller-owned notification preferences",
}

// readOnlyPosts are POST operations that only mint a short-lived challenge and
// change no persistent state, so they need no Tier 1 audit row. The key is
// "METHOD path"; an entry needs a reviewed reason.
var readOnlyPosts = map[string]string{
	"POST /api/v1/me/mfa/passkeys/options": "issues a five-minute WebAuthn registration challenge",
	"POST /api/v1/auth/passkeys/options":   "issues a five-minute WebAuthn assertion challenge",
}

// customVerbs is the closed set of AIP-136-style custom methods. The project
// uses the /resource/{id}/verb spelling (not /resource/{id}:verb) because Fiber
// reserves ':' for path parameters.
var customVerbs = map[string]bool{
	"restore": true, "rotate-secret": true, "replay": true, "step-up": true,
	"complete": true, "verify": true, "accept": true, "signup": true, "exchange": true,
	"confirm": true, "rotate": true,
}

// legacyVerbSegments are the pre-huma RPC spellings. They are rejected so the
// route map's one custom-method syntax actually holds.
var legacyVerbSegments = map[string]string{
	"upload":     "create the resource with POST /files",
	"download":   "expose the bytes as /content",
	"revoke":     "use DELETE on the resource or PATCH a status field",
	"ban":        "PATCH /members/{id} with {status}",
	"unban":      "PATCH /members/{id} with {status}",
	"send":       "POST /members/{id}/notifications",
	"read":       "PATCH the notification's read field",
	"read-all":   "PATCH the notifications collection",
	"enroll":     "POST /me/mfa/totp",
	"regenerate": "POST /me/mfa/recovery-codes",
	"status":     "PATCH the resource with {status}",
}

var singularSegments = map[string]bool{
	"user": true, "member": true, "session": true, "role": true, "invitation": true,
	"tenant": true, "file": true, "notification": true, "webhook": true, "api-key": true,
	"data-export": true, "erasure-request": true, "feature-flag": true, "passkey": true,
}

func TestEveryOperationIsPublicOrHasPermission(t *testing.T) {
	ops := archOperations(t)
	modules, _ := productionModules(t)
	catalogs, err := app.BuildCatalogs(modules...)
	if err != nil {
		t.Fatalf("build catalogs: %v", err)
	}

	for _, op := range ops {
		key := op.Op.Method + " " + op.Op.Path
		if op.Policy.Public {
			if reason, ok := publicOperations[key]; !ok || strings.TrimSpace(reason) == "" {
				t.Errorf("%s is public but not in publicOperations with a reason", key)
			}
			if op.Policy.Permission != "" || len(op.Policy.AnyOf) > 0 {
				t.Errorf("%s is public and also checks a permission", key)
			}
			continue
		}
		if op.Policy.Permission == "" && len(op.Policy.AnyOf) == 0 {
			t.Errorf("%s is neither public nor protected by a permission", key)
		}
		for _, permission := range append([]authz.Permission{op.Policy.Permission}, op.Policy.AnyOf...) {
			if permission != "" && !catalogs.Permissions.IsKnownPermission(permission) {
				t.Errorf("%s uses unknown permission %q", key, permission)
			}
		}
	}
}

func TestEveryMutatingOperationIsTier1OrGuarded(t *testing.T) {
	ops := archOperations(t)
	for _, op := range ops {
		if !mutatingOperation(op) {
			continue
		}
		key := op.Op.Method + " " + op.Op.Path
		switch {
		case op.Policy.Audit.Tier1Action != "" && op.Policy.Audit.Guard != nil:
			t.Errorf("%s declares both Tier 1 and Guard; choose one", key)
		case op.Policy.Audit.Tier1Action != "":
			// A service records this action in the mutation transaction.
		case op.Policy.Audit.Guard != nil:
			if strings.TrimSpace(op.Policy.Audit.Guard.Action) == "" {
				t.Errorf("%s Guard has no action", key)
			}
			if strings.TrimSpace(op.Policy.Audit.Guard.Resource) == "" {
				t.Errorf("%s Guard has no resource", key)
			}
			if op.Policy.Audit.Guard.Mode == audithttp.ModeBestEffort {
				if reason, ok := bestEffortGuards[key]; !ok || strings.TrimSpace(reason) == "" {
					t.Errorf("%s uses a best-effort Guard; privileged mutations must be fail-closed or listed with a reason", key)
				}
			}
		default:
			t.Errorf("%s is mutating but neither Tier 1 audited nor Guarded", key)
		}
	}
}

// mutatingOperation includes the Google callback: it is a GET that creates a
// session, so it needs the same audit guarantee as a POST.
func mutatingOperation(op httpapi.OperationInfo) bool {
	if op.Op.Method == http.MethodPost && readOnlyPosts[op.Op.Method+" "+op.Op.Path] != "" {
		return false
	}
	if httpapi.Mutating(op.Op.Method) {
		return true
	}
	return op.Op.Method == "GET" && strings.HasSuffix(op.Op.Path, "/auth/google/callback")
}

func TestPlatformPermissionsImplyMFA(t *testing.T) {
	ops := archOperations(t)
	platformOps := 0
	for _, op := range ops {
		requiresPlatform := strings.HasPrefix(string(op.Policy.Permission), "platform:")
		for _, permission := range op.Policy.AnyOf {
			if strings.HasPrefix(string(permission), "platform:") {
				requiresPlatform = true
			}
		}
		if !requiresPlatform {
			continue
		}
		platformOps++
		if !op.Policy.RequireMFA {
			t.Errorf("%s %s accepts a platform permission but RequireMFA is false", op.Op.Method, op.Op.Path)
		}
		if !op.Policy.RequirePasskey {
			t.Errorf("%s %s accepts a platform permission but RequirePasskey is false", op.Op.Method, op.Op.Path)
		}
	}
	if platformOps == 0 {
		t.Fatal("no registered operation accepts a platform permission; extend the stub or fix the policy")
	}
}

func TestOperationKeysAndPathsAreUnique(t *testing.T) {
	ops := archOperations(t)
	routes := map[string]bool{}
	ids := map[string]bool{}
	for _, op := range ops {
		key := op.Op.Method + " " + op.Op.Path
		if routes[key] {
			t.Errorf("duplicate operation %s", key)
		}
		routes[key] = true
		if ids[op.Op.OperationID] {
			t.Errorf("duplicate operationId %q", op.Op.OperationID)
		}
		ids[op.Op.OperationID] = true
	}
}

func TestModulePathsFollowNamingRules(t *testing.T) {
	ops := archOperations(t)
	for _, op := range ops {
		if !strings.HasPrefix(op.Op.Path, httpapi.APIPrefix+"/") {
			if op.Op.Infrastructure && infrastructurePaths[op.Op.Path] {
				continue
			}
			t.Errorf("%s %s: only the reviewed probes may live outside %s/", op.Op.Method, op.Op.Path, httpapi.APIPrefix)
			continue
		}
		key := op.Op.Method + " " + op.Op.Path
		if err := httpapi.ValidatePath(op.Op.Path); err != nil {
			t.Errorf("%s: %v", key, err)
		}
		if strings.Contains(op.Op.Path, "//") || strings.HasSuffix(op.Op.Path, "/") {
			t.Errorf("%s: path must not have empty or trailing segments", key)
		}
		if strings.Contains(op.Op.Path, "/admin/") {
			t.Errorf("%s: /admin is retired; use /me, /members, /roles, /webhooks, or /platform", key)
		}
		if strings.Contains(op.Op.Path, "/api/v2") {
			t.Errorf("%s: only /api/v1 is mounted", key)
		}
		if len(op.Op.Tags) == 0 || strings.TrimSpace(op.Op.Summary) == "" {
			t.Errorf("%s: tags and summary are required for the OpenAPI document", key)
		}
		segments := strings.Split(strings.TrimPrefix(op.Op.Path, "/"), "/")
		for i, segment := range segments {
			if strings.HasPrefix(segment, "{") {
				continue
			}
			if singularSegments[segment] {
				t.Errorf("%s: collection %q must be plural", key, segment)
			}
			if i != len(segments)-1 {
				continue
			}
			if legacyVerbSegments[segment] != "" {
				t.Errorf("%s: final %q is a retired RPC verb; %s", key, segment, legacyVerbSegments[segment])
			}
			if customVerbs[segment] && op.Op.Method != "POST" {
				t.Errorf("%s: custom method %q must use POST", key, segment)
			}
		}
	}
}

// nonModuleDirs are directories under internal/modules deliberately not
// registered in internal/bootstrap. There are none today.
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

// TestModuleNamesAreUnique keeps the registry and the catalogs in step:
// RegisterCatalogs owns entries by Module.Name(), so names must be unique and
// stable.
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

// TestModuleRegistrationIsRoutedThroughHuma keeps every module on the
// single huma registration path.
func TestModuleRegistrationIsRoutedThroughHuma(t *testing.T) {
	modules, _ := productionModules(t)
	for _, m := range modules {
		if _, ok := m.(app.RouteProvider); !ok {
			t.Errorf("module %s does not implement RouteProvider", m.Name())
		}
	}
}
