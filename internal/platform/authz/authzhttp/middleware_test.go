package authzhttp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

// newTestApp builds an app whose pre-middleware copies the X-Test-Role header
// into Fiber locals exactly as JWTAuth does in production.
func newTestApp() *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if role := c.Get("X-Test-Role"); role != "" {
			// Mimic the session store: the effective permission set, never the
			// role name alone, is what Require checks.
			set := testCatalog.PermissionsForRole(role)
			permissions := make([]authz.Permission, 0, len(set))
			for permission := range set {
				permissions = append(permissions, permission)
			}
			SetSubject(c, authz.NewSubjectWithPermissions(uuid.Nil, uuid.Nil, role, permissions...))
		}
		return c.Next()
	})
	return app
}

func doGet(t *testing.T, app *fiber.App, path, role string) (status int, contentType string, body []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

func decodeProblem(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var problem map[string]any
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode problem JSON: %v (body=%s)", err, body)
	}
	return problem
}

func assertForbidden(t *testing.T, status int, contentType string, body []byte) {
	t.Helper()

	if status != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", status, body)
	}
	if contentType != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", contentType)
	}
	problem := decodeProblem(t, body)
	if problem["code"] != "permission.denied" {
		t.Errorf("code = %v, want permission.denied", problem["code"])
	}
	if problem["detail"] != "insufficient permissions" {
		t.Errorf("detail = %v, want insufficient permissions", problem["detail"])
	}
}

func TestRequire_AllowsGrantedPermission(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/files", Require(testPermissionRead), func(c fiber.Ctx) error {
		called = true
		return c.SendString("ok")
	})

	status, _, body := doGet(t, app, "/files", authz.RoleMember)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
	if !called {
		t.Error("protected handler was not called")
	}
}

func TestRequire_AllowsAdminOnAdminPermission(t *testing.T) {
	app := newTestApp()
	app.Get("/admin/users", Require(authz.PermissionAdminUsersRead), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	status, _, body := doGet(t, app, "/admin/users", authz.RoleAdmin)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
}

func TestRequire_DeniesUserOnAdminPermission(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/admin/users", Require(authz.PermissionAdminUsersRead), func(c fiber.Ctx) error {
		called = true
		return c.SendString("ok")
	})

	status, contentType, body := doGet(t, app, "/admin/users", authz.RoleMember)
	assertForbidden(t, status, contentType, body)
	if called {
		t.Error("protected handler ran despite denied permission")
	}
}

func TestRequire_DeniesMissingRole(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/profile", Require(testPermissionWrite), func(c fiber.Ctx) error {
		called = true
		return c.SendString("ok")
	})

	status, contentType, body := doGet(t, app, "/profile", "")
	assertForbidden(t, status, contentType, body)
	if called {
		t.Error("protected handler ran without an authenticated role")
	}
}

func TestRequire_DeniesUnknownRole(t *testing.T) {
	app := newTestApp()
	app.Get("/profile", Require(testPermissionWrite), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	status, contentType, body := doGet(t, app, "/profile", "guest")
	assertForbidden(t, status, contentType, body)
}

func TestRequireAny_AllowsAnyGrantedPermission(t *testing.T) {
	app := newTestApp()
	app.Get("/either",
		RequireAny(authz.PermissionAdminStatsRead, testPermissionRead),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	status, _, body := doGet(t, app, "/either", authz.RoleMember)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
}

func TestRequireAny_DeniesWhenNoPermissionMatches(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/either",
		RequireAny(authz.PermissionAdminStatsRead, authz.PermissionAdminUsersRole),
		func(c fiber.Ctx) error {
			called = true
			return c.SendString("ok")
		},
	)

	status, contentType, body := doGet(t, app, "/either", authz.RoleMember)
	assertForbidden(t, status, contentType, body)
	if called {
		t.Error("protected handler ran despite no matching permission")
	}
}

func TestRequireAny_EmptyListDeniesEvenAdmin(t *testing.T) {
	app := newTestApp()
	app.Get("/either", RequireAny(), func(c fiber.Ctx) error { return c.SendString("ok") })

	status, contentType, body := doGet(t, app, "/either", authz.RoleAdmin)
	assertForbidden(t, status, contentType, body)
}

func TestRequireAny_DeniesMissingRole(t *testing.T) {
	app := newTestApp()
	app.Get("/either",
		RequireAny(testPermissionWrite, authz.PermissionAdminStatsRead),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	status, contentType, body := doGet(t, app, "/either", "")
	assertForbidden(t, status, contentType, body)
}

func TestRequire_UsesExplicitSubjectScopes(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		// An administrator's API key limited to files:read must not inherit the
		// administrator role catalog.
		SetSubject(c, authz.NewSubjectWithPermissions(testID(1), testID(1), authz.RoleAdmin, testPermissionRead))
		return c.Next()
	})
	app.Get("/files", Require(testPermissionRead), func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/admin", Require(authz.PermissionAdminUsersRead), func(c fiber.Ctx) error { return c.SendString("ok") })

	status, _, body := doGet(t, app, "/files", "")
	if status != fiber.StatusOK {
		t.Fatalf("files status = %d, want 200 (body=%s)", status, body)
	}
	status, contentType, body := doGet(t, app, "/admin", "")
	assertForbidden(t, status, contentType, body)
}

func TestRequireMFA(t *testing.T) {
	fresh := time.Now()
	cases := map[string]struct {
		amr        []string
		verifiedAt time.Time
		want       int
	}{
		"password only":            {amr: []string{"pwd"}, want: fiber.StatusForbidden},
		"no amr":                   {want: fiber.StatusForbidden},
		"totp fresh":               {amr: []string{"pwd", "otp"}, verifiedAt: fresh, want: fiber.StatusOK},
		"recovery code fresh":      {amr: []string{"google", "rc"}, verifiedAt: fresh, want: fiber.StatusOK},
		"passkey hardware fresh":   {amr: []string{"hwk"}, verifiedAt: fresh, want: fiber.StatusOK},
		"passkey software fresh":   {amr: []string{"swk"}, verifiedAt: fresh, want: fiber.StatusOK},
		"totp older than max age":  {amr: []string{"pwd", "otp"}, verifiedAt: fresh.Add(-13 * time.Hour), want: fiber.StatusForbidden},
		"totp from the future":     {amr: []string{"pwd", "otp"}, verifiedAt: fresh.Add(time.Hour), want: fiber.StatusForbidden},
		"totp without a timestamp": {amr: []string{"pwd", "otp"}, want: fiber.StatusForbidden},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
			app.Use(func(c fiber.Ctx) error {
				SetSubject(c, authz.NewSubject(testID(1), testID(1), authz.RoleMember).
					WithAMR(tc.amr).
					WithMFAVerifiedAt(tc.verifiedAt))
				return c.Next()
			})
			app.Get("/secure", RequireMFA(12*time.Hour), func(c fiber.Ctx) error { return c.SendString("ok") })

			status, _, body := doGet(t, app, "/secure", "")
			if status != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", status, tc.want, body)
			}
			if tc.want == fiber.StatusForbidden {
				if !json.Valid(body) {
					t.Fatalf("body = %s, want a problem document", body)
				}
				if !strings.Contains(string(body), string(apperror.CodeMFARequired)) {
					t.Fatalf("body = %s, want code %q", body, apperror.CodeMFARequired)
				}
			}
		})
	}
}

func TestRequireMFADeniesAnonymous(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/secure", RequireMFA(time.Hour), func(c fiber.Ctx) error { return c.SendString("ok") })
	if status, _, body := doGet(t, app, "/secure", ""); status != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", status, body)
	}
}

// TestRequirePasskeyMFA proves the platform gate cannot be satisfied by a
// phishable TOTP or recovery code, only by a passkey.
func TestRequirePasskeyMFA(t *testing.T) {
	fresh := time.Now()
	cases := map[string]struct {
		amr        []string
		verifiedAt time.Time
		want       int
	}{
		"password only":          {amr: []string{"pwd"}, want: fiber.StatusForbidden},
		"totp fresh":             {amr: []string{"pwd", "otp"}, verifiedAt: fresh, want: fiber.StatusForbidden},
		"recovery code fresh":    {amr: []string{"google", "rc"}, verifiedAt: fresh, want: fiber.StatusForbidden},
		"hardware passkey":       {amr: []string{"pwd", "hwk"}, verifiedAt: fresh, want: fiber.StatusOK},
		"software passkey":       {amr: []string{"pwd", "swk"}, verifiedAt: fresh, want: fiber.StatusOK},
		"stale hardware passkey": {amr: []string{"pwd", "hwk"}, verifiedAt: fresh.Add(-13 * time.Hour), want: fiber.StatusForbidden},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
			app.Use(func(c fiber.Ctx) error {
				SetSubject(c, authz.NewSubject(testID(1), testID(1), authz.RoleAdmin).
					WithAMR(tc.amr).
					WithMFAVerifiedAt(tc.verifiedAt))
				return c.Next()
			})
			app.Get("/platform", RequirePasskeyMFA(12*time.Hour), func(c fiber.Ctx) error { return c.SendString("ok") })
			status, _, body := doGet(t, app, "/platform", "")
			if status != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", status, tc.want, body)
			}
			if tc.want == fiber.StatusForbidden && !strings.Contains(string(body), string(apperror.CodeMFARequired)) {
				t.Fatalf("body = %s, want code %q", body, apperror.CodeMFARequired)
			}
		})
	}
}

// testID returns a deterministic UUID for a small fixture number.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}
