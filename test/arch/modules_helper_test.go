package arch_test

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

const archTestSecret = "arch-route-coverage-secret-0123456789"

// testIdempotency is a named marker so the coverage tests can find the
// idempotency position in a handler chain.
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

// productionModules builds every module from the same ordered list cmd/api
// uses (internal/bootstrap). Construction performs no I/O, so nil
// infrastructure is enough to mount and inspect the route tables. A module
// added to bootstrap is covered by every route test automatically.
func productionModules(t *testing.T) ([]app.Module, app.Deps) {
	t.Helper()
	cfg := &config.Config{
		JWT: config.JWTConfig{Secret: archTestSecret, ExpireHour: 1, RefreshExpireDays: 1},
	}
	modules, err := bootstrap.Build(app.Infra{Config: cfg})
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	if err := app.RegisterCatalogs(modules.List...); err != nil {
		t.Fatalf("RegisterCatalogs: %v", err)
	}
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
	return modules.List, deps
}

// mountedRoute is one route as registered by one module.
type mountedRoute struct {
	module string
	route  fiber.Route
}

// moduleRoutes mounts each module on its own Fiber app under /api/v1 so every
// route (and every middleware-bearing group prefix) is attributed to the
// module that registered it.
func moduleRoutes(t *testing.T) (endpoints, groupMiddleware []mountedRoute) {
	t.Helper()
	modules, deps := productionModules(t)
	for _, m := range modules {
		fiberApp := fiber.New()
		m.RegisterRoutes(fiberApp.Group("/api/v1"), deps)

		// GetRoutes(false) additionally lists Use() registrations, which is
		// how Fiber mounts group middleware (Group(prefix, handlers...)).
		// Subtracting the endpoint multiset leaves exactly those entries.
		endpointCount := map[string]int{}
		for _, route := range fiberApp.GetRoutes(true) {
			endpointCount[routeKey(route)]++
			endpoints = append(endpoints, mountedRoute{module: m.Name(), route: route})
		}
		seenUse := map[string]bool{}
		for _, route := range fiberApp.GetRoutes(false) {
			key := routeKey(route)
			if endpointCount[key] > 0 {
				endpointCount[key]--
				continue
			}
			if seenUse[route.Path] {
				continue
			}
			seenUse[route.Path] = true
			groupMiddleware = append(groupMiddleware, mountedRoute{module: m.Name(), route: route})
		}
	}
	return endpoints, groupMiddleware
}

// routeKey identifies a registration by method, path, and handler chain.
func routeKey(route fiber.Route) string {
	key := route.Method + " " + route.Path
	for _, handler := range route.Handlers {
		key += " " + handlerName(handler)
	}
	return key
}

// handlerChainContains reports whether any handler's function name contains
// one of the fragments.
func handlerChainContains(route fiber.Route, fragments ...string) bool {
	for _, handler := range route.Handlers {
		name := handlerName(handler)
		for _, fragment := range fragments {
			if strings.Contains(name, fragment) {
				return true
			}
		}
	}
	return false
}
