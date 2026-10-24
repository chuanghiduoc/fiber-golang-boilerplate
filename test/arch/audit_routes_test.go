package arch_test

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

const guardTestSecret = "audit-route-coverage-secret"

// tier1Mutations are audited inside the service's database transaction. They
// must not also carry a route-level Guard intent row.
var tier1Mutations = map[string]bool{
	"POST /api/v1/auth/register":           true,
	"PUT /api/v1/users/me":                 true,
	"PUT /api/v1/users/me/password":        true,
	"PUT /api/v1/users/:id":                true,
	"DELETE /api/v1/users/:id":             true,
	"PUT /api/v1/admin/users/:id/role":     true,
	"POST /api/v1/admin/users/:id/ban":     true,
	"POST /api/v1/admin/users/:id/unban":   true,
	"DELETE /api/v1/admin/files/:id":       true,
	"POST /api/v1/admin/files/:id/restore": true,
}

// testIdempotency is a named marker so the coverage test can find the
// idempotency position in the handler chain.
func testIdempotency(c fiber.Ctx) error { return c.Next() }

func passthrough(c fiber.Ctx) error { return c.Next() }

func handlerName(handler fiber.Handler) string {
	if handler == nil {
		return ""
	}
	fn := runtime.FuncForPC(reflect.ValueOf(handler).Pointer())
	if fn == nil {
		return ""
	}
	return fn.Name()
}

func TestEveryMutatingRouteIsTier1OrGuarded(t *testing.T) {
	cfg := &config.Config{JWT: config.JWTConfig{Secret: guardTestSecret}}
	deps := app.Deps{
		Config: cfg,
		Limiters: app.Limiters{
			Strict:  passthrough,
			Normal:  passthrough,
			Relaxed: passthrough,
		},
		Idempotency: testIdempotency,
		Audit:       audit.Noop{},
	}

	fiberApp := fiber.New()
	v1 := fiberApp.Group("/api/v1")
	identity.New(
		identityhandler.NewAuthHandler(nil, nil, nil, nil, guardTestSecret, 24, false, nil),
		identityhandler.NewUserHandler(nil),
	).RegisterRoutes(v1, deps)
	files.New(fileshandler.NewUploadHandler(nil, 0, nil)).RegisterRoutes(v1, deps)
	admin.New(adminhandler.NewHandler(nil)).RegisterRoutes(v1, deps)

	seenTier1 := make(map[string]bool, len(tier1Mutations))
	for _, route := range fiberApp.GetRoutes(true) {
		mutating := route.Method == fiber.MethodPost ||
			route.Method == fiber.MethodPut ||
			route.Method == fiber.MethodPatch ||
			route.Method == fiber.MethodDelete ||
			(route.Method == fiber.MethodGet && strings.HasSuffix(route.Path, "/google/callback"))
		if !mutating {
			continue
		}

		key := route.Method + " " + route.Path
		if tier1Mutations[key] {
			seenTier1[key] = true
			continue
		}

		guardIndex := -1
		idempotencyIndex := -1
		for i, handler := range route.Handlers {
			name := handlerName(handler)
			if strings.Contains(name, "audit.Guard") {
				guardIndex = i
			}
			if strings.Contains(name, "testIdempotency") {
				idempotencyIndex = i
			}
		}
		if guardIndex < 0 {
			t.Errorf("mutating route %s is neither Tier 1 nor wrapped by audit.Guard", key)
			continue
		}
		if idempotencyIndex >= 0 && idempotencyIndex > guardIndex {
			t.Errorf("route %s must run idempotency before audit.Guard (idempotency=%d guard=%d)",
				key, idempotencyIndex, guardIndex)
		}
	}

	for key := range tier1Mutations {
		if !seenTier1[key] {
			t.Errorf("Tier 1 route %s is missing; update the coverage map with the real route", key)
		}
	}
}
