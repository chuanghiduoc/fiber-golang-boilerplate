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

// finalHandler stands in for a route's endpoint in synthetic route tests.
func finalHandler(fiber.Ctx) error { return nil }

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

// productionBootstrap builds the same ordered module set cmd/api uses
// (internal/bootstrap). Tests that inspect composition-root wiring (the
// tenancy registry) use this directly; productionModules keeps the route-test
// signature.
func productionBootstrap(t *testing.T) *bootstrap.Modules {
	t.Helper()
	cfg := &config.Config{
		JWT: config.JWTConfig{Secret: archTestSecret, ExpireHour: 1, RefreshExpireDays: 1},
	}
	modules, err := bootstrap.Build(cfg, app.Infra{Platform: app.NewPlatformConfig(cfg)})
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	if err := app.RegisterCatalogs(modules.List...); err != nil {
		t.Fatalf("RegisterCatalogs: %v", err)
	}
	return modules
}

// productionModules builds every module from the same ordered list cmd/api
// uses (internal/bootstrap). Construction performs no I/O, so nil
// infrastructure is enough to mount and inspect the route tables. A module
// added to bootstrap is covered by every route test automatically.
func productionModules(t *testing.T) ([]app.Module, app.Deps) {
	t.Helper()
	modules := productionBootstrap(t)
	cfg := &config.Config{
		JWT: config.JWTConfig{Secret: archTestSecret, ExpireHour: 1, RefreshExpireDays: 1},
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

// apiPrefixes are the versioned groups app.Register mounts modules under.
var apiPrefixes = []string{"/api/v1", "/api/v2"}

// underAPIPrefix reports whether path is below a versioned API group.
func underAPIPrefix(path string) bool {
	for _, prefix := range apiPrefixes {
		if strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// moduleRoutes mounts each module on its own Fiber app under /api/v1 (and,
// for app.V2Module implementations, /api/v2, exactly like app.Register) so
// every route (and every middleware-bearing group prefix) is attributed to
// the module that registered it.
func moduleRoutes(t *testing.T) (endpoints, groupMiddleware []mountedRoute) {
	t.Helper()
	modules, deps := productionModules(t)
	for _, m := range modules {
		fiberApp := fiber.New()
		m.RegisterRoutes(fiberApp.Group("/api/v1"), deps)
		if v2, ok := m.(app.V2Module); ok {
			v2.RegisterV2Routes(fiberApp.Group("/api/v2"), deps)
		}

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

// modulePath is the Go module path; handler names are matched by full import
// path so a local package that happens to be named authz or audit cannot
// satisfy a check.
const modulePath = "github.com/chuanghiduoc/fiber-golang-boilerplate"

// isPlatformFunc reports whether a runtime function name is fn (or one of its
// closures) in pkg, e.g. isPlatformFunc(name, "internal/platform/authz/authzhttp",
// "Require") matches ".../internal/platform/authz/authzhttp.Require.func1".
func isPlatformFunc(name, pkg, fn string) bool {
	full := modulePath + "/" + pkg + "." + fn
	return name == full || strings.HasPrefix(name, full+".")
}

// handlerChainHas reports whether any handler in the chain is fn in pkg.
func handlerChainHas(route fiber.Route, pkg string, fns ...string) bool {
	return handlerIndex(route, pkg, fns...) >= 0
}

// handlerBeforeFinal reports whether fn in pkg appears in the chain before
// the route's last handler (index < len(Handlers)-1). Fiber runs the chain in
// order and the last handler is the endpoint, so middleware after it never
// guards it.
func handlerBeforeFinal(route fiber.Route, pkg string, fns ...string) bool {
	for i := 0; i < len(route.Handlers)-1; i++ {
		name := handlerName(route.Handlers[i])
		for _, fn := range fns {
			if isPlatformFunc(name, pkg, fn) {
				return true
			}
		}
	}
	return false
}

// handlerIndex returns the position of the last handler that is fn in pkg,
// or -1.
func handlerIndex(route fiber.Route, pkg string, fns ...string) int {
	index := -1
	for i, handler := range route.Handlers {
		name := handlerName(handler)
		for _, fn := range fns {
			if isPlatformFunc(name, pkg, fn) {
				index = i
			}
		}
	}
	return index
}
