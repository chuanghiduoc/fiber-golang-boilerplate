package authz

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// newTestApp builds an app whose pre-middleware copies the X-Test-Role header
// into Fiber locals exactly as JWTAuth does in production.
func newTestApp() *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if role := c.Get("X-Test-Role"); role != "" {
			fiber.Locals[string](c, "role", role)
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
	app.Get("/files", Require(PermissionFilesRead), func(c fiber.Ctx) error {
		called = true
		return c.SendString("ok")
	})

	status, _, body := doGet(t, app, "/files", RoleUser)
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
	app.Get("/admin/users", Require(PermissionAdminUsersRead), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	status, _, body := doGet(t, app, "/admin/users", RoleAdmin)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
}

func TestRequire_DeniesUserOnAdminPermission(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/admin/users", Require(PermissionAdminUsersRead), func(c fiber.Ctx) error {
		called = true
		return c.SendString("ok")
	})

	status, contentType, body := doGet(t, app, "/admin/users", RoleUser)
	assertForbidden(t, status, contentType, body)
	if called {
		t.Error("protected handler ran despite denied permission")
	}
}

func TestRequire_DeniesMissingRole(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/profile", Require(PermissionProfileRead), func(c fiber.Ctx) error {
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
	app.Get("/profile", Require(PermissionProfileRead), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	status, contentType, body := doGet(t, app, "/profile", "guest")
	assertForbidden(t, status, contentType, body)
}

func TestRequireAny_AllowsAnyGrantedPermission(t *testing.T) {
	app := newTestApp()
	app.Get("/either",
		RequireAny(PermissionAdminStatsRead, PermissionFilesRead),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	status, _, body := doGet(t, app, "/either", RoleUser)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", status, body)
	}
}

func TestRequireAny_DeniesWhenNoPermissionMatches(t *testing.T) {
	app := newTestApp()
	called := false
	app.Get("/either",
		RequireAny(PermissionAdminStatsRead, PermissionAdminUsersRole),
		func(c fiber.Ctx) error {
			called = true
			return c.SendString("ok")
		},
	)

	status, contentType, body := doGet(t, app, "/either", RoleUser)
	assertForbidden(t, status, contentType, body)
	if called {
		t.Error("protected handler ran despite no matching permission")
	}
}

func TestRequireAny_EmptyListDeniesEvenAdmin(t *testing.T) {
	app := newTestApp()
	app.Get("/either", RequireAny(), func(c fiber.Ctx) error { return c.SendString("ok") })

	status, contentType, body := doGet(t, app, "/either", RoleAdmin)
	assertForbidden(t, status, contentType, body)
}

func TestRequireAny_DeniesMissingRole(t *testing.T) {
	app := newTestApp()
	app.Get("/either",
		RequireAny(PermissionProfileRead, PermissionAdminStatsRead),
		func(c fiber.Ctx) error { return c.SendString("ok") },
	)

	status, contentType, body := doGet(t, app, "/either", "")
	assertForbidden(t, status, contentType, body)
}
