package admin_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// TestAdminRoutesDoNotWrapSiblingModules guards against registering auth or
// the rate limiter as /admin group middleware. Fiber installs group
// middleware as a prefix Use, so it would also run on /admin/features/* and
// /admin/api-keys/*, which other modules own and protect themselves.
func TestAdminRoutesDoNotWrapSiblingModules(t *testing.T) {
	adminLimiterHits := 0
	deps := app.Deps{
		Config: &config.Config{JWT: config.JWTConfig{Secret: "admin-module-test-secret"}},
		Limiters: app.Limiters{Normal: func(c fiber.Ctx) error {
			adminLimiterHits++
			return c.Next()
		}},
		Idempotency: func(c fiber.Ctx) error { return c.Next() },
	}

	fiberApp := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	v1 := fiberApp.Group("/api/v1")
	admin.New(adminhandler.NewHandler(nil)).RegisterRoutes(v1, deps)
	// Stand-ins for the features and apikeys modules, registered after admin
	// exactly as cmd/api does.
	sibling := func(c fiber.Ctx) error { return c.SendString("sibling") }
	v1.Get("/admin/features/flags", sibling)
	v1.Get("/admin/api-keys", sibling)

	for _, path := range []string{"/api/v1/admin/features/flags", "/api/v1/admin/api-keys"} {
		resp, err := fiberApp.Test(httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("GET %s status = %d, want 200: admin auth must not run on sibling module routes", path, resp.StatusCode)
		}
	}
	if adminLimiterHits != 0 {
		t.Errorf("admin limiter ran %d times on sibling module routes, want 0", adminLimiterHits)
	}

	// The admin routes themselves stay protected.
	resp, err := fiberApp.Test(httptest.NewRequest(http.MethodGet, "/api/v1/admin/stats", http.NoBody))
	if err != nil {
		t.Fatalf("GET /admin/stats: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("GET /admin/stats without a token status = %d, want 401", resp.StatusCode)
	}
}
