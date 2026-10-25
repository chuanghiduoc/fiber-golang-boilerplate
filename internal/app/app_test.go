package app_test

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
)

type pingModule struct{}

func (pingModule) Name() string { return "ping" }
func (pingModule) RegisterRoutes(v1 fiber.Router, _ app.Deps) {
	v1.Get("/ping", func(c fiber.Ctx) error { return c.SendString("pong") })
}

type flagModule struct {
	enabled func(app.Deps) bool
}

func (m flagModule) Name() string { return "flag" }

// FeatureFlags implements app.FlagProvider so Register adds the key to the
// catalog before any request evaluates it.
func (m flagModule) FeatureFlags() []features.Definition {
	return []features.Definition{{Key: testFlagKey, Description: "app test flag", Default: true}}
}

const testFlagKey features.Key = "app_test_flag"

func (m flagModule) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	v1.Get("/flag", func(c fiber.Ctx) error {
		if m.enabled(deps) {
			return c.SendString("true")
		}
		return c.SendString("false")
	})
}

type staticTenantResolver struct{}

func (staticTenantResolver) Resolve(_ context.Context, slug string) (tenant.Tenant, error) {
	if slug == "default" {
		return tenant.Tenant{ID: 1, Slug: "default", Name: "Default"}, nil
	}
	return tenant.Tenant{}, apperror.ErrNotFound
}

func TestModuleRegistryMountsRoutes(t *testing.T) {
	cfg := &config.Config{
		// production disables the Swagger file loader, which resolves relative
		// to the process working directory (the package dir during go test).
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(pingModule{})

	req, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAppProvidesNilSafeFeatureEvaluator(t *testing.T) {
	cfg := &config.Config{
		App:          config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:         config.CORSConfig{AllowOrigins: "*"},
		RateLimit:    config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:       config.TenantConfig{DefaultSlug: "default"},
		FeatureFlags: config.FeatureFlagsConfig{Flags: "app_test_flag=false"},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(flagModule{enabled: func(deps app.Deps) bool {
		if deps.Flags == nil {
			t.Fatal("app.New must install a default feature evaluator")
		}
		return deps.Flags.Enabled(context.Background(), testFlagKey, features.Subject{})
	}})

	req, _ := http.NewRequest("GET", "/api/v1/flag", http.NoBody)
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "false", string(body), "FEATURE_FLAGS must apply when Deps.Flags is nil")
	assert.True(t, features.IsKnown(testFlagKey), "Register must add FlagProvider keys to the catalog")
}

// catalogModule exercises every catalog provider hook.
type catalogModule struct {
	name       string
	permission authz.Permission
	code       apperror.Code
	flag       features.Key
	locales    fstest.MapFS
}

func (m catalogModule) Name() string                          { return m.name }
func (m catalogModule) RegisterRoutes(fiber.Router, app.Deps) {}
func (m catalogModule) Permissions() []authz.Grant {
	return []authz.Grant{{Permission: m.permission, Roles: []string{authz.RoleUser}}}
}
func (m catalogModule) ErrorCodes() map[apperror.Code]apperror.Spec {
	return map[apperror.Code]apperror.Spec{m.code: {Status: 409, Title: "Gadget busy", DocPath: "/errors/gadget-busy"}}
}
func (m catalogModule) FeatureFlags() []features.Definition {
	return []features.Definition{{Key: m.flag, Description: "gadget flag"}}
}
func (m catalogModule) Locales() fs.FS { return m.locales }

func TestRegisterCatalogsWiresEveryProvider(t *testing.T) {
	module := catalogModule{
		name:       "gadgets",
		permission: "gadgets:read",
		code:       "gadget.busy",
		flag:       "gadgets_beta",
		locales: fstest.MapFS{
			"en.json": {Data: []byte(`{"errors.gadget.busy.title": "Gadget busy"}`)},
			"vi.json": {Data: []byte(`{"errors.gadget.busy.title": "Thiết bị bận"}`)},
		},
	}
	require.NoError(t, app.RegisterCatalogs(module))
	require.NoError(t, app.RegisterCatalogs(module), "re-registering the same module is a no-op")

	assert.True(t, authz.Has(authz.RoleUser, "gadgets:read"))
	assert.True(t, authz.Has(authz.RoleAdmin, "gadgets:read"), "admins receive every permission")
	spec, ok := apperror.SpecFor("gadget.busy")
	require.True(t, ok)
	assert.Equal(t, 409, spec.Status)
	assert.True(t, features.IsKnown("gadgets_beta"))

	bundle, err := i18n.New("en", app.LocaleSources(module)...)
	require.NoError(t, err)
	assert.Equal(t, "Thiết bị bận", bundle.Translate("vi", "errors.gadget.busy.title", nil))
}

func TestRegisterCatalogsRejectsConflicts(t *testing.T) {
	owner := catalogModule{name: "owner_mod", permission: "ownermod:read", code: "ownermod.busy", flag: "ownermod_beta"}
	require.NoError(t, app.RegisterCatalogs(owner))

	thief := owner
	thief.name = "thief_mod"
	err := app.RegisterCatalogs(thief)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already owned")

	err = app.RegisterCatalogs(pingModule{}, pingModule{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered twice")

	application := app.New(&config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}, app.Deps{})
	assert.Panics(t, func() { application.Register(thief) }, "a catalog conflict at Register is a programming error")
}

func TestTenantMiddlewareRejectsUnknownHeader(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{TenantResolver: staticTenantResolver{}, Health: health.NewChecker(nil, nil)})
	application.Register(pingModule{})

	withHeader, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	withHeader.Header.Set(tenant.Header, "missing")
	resp, err := application.Fiber().Test(withHeader)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	withoutHeader, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	resp, err = application.Fiber().Test(withoutHeader)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Infrastructure probes are outside the tenant scope.
	healthReq, _ := http.NewRequest("GET", "/healthz", http.NoBody)
	resp, err = application.Fiber().Test(healthReq)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestTenantMiddlewareRequiresHeaderWhenConfigured(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default", RequireHeader: true},
	}
	application := app.New(cfg, app.Deps{TenantResolver: staticTenantResolver{}})
	application.Register(pingModule{})

	withoutHeader, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	resp, err := application.Fiber().Test(withoutHeader)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	withHeader, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	withHeader.Header.Set(tenant.Header, "default")
	resp, err = application.Fiber().Test(withHeader)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestCORSPreflightAllowsIdempotencyKey(t *testing.T) {
	t.Setenv("CORS_ALLOW_HEADERS", "")
	t.Setenv("CORS_EXPOSE_HEADERS", "")
	var corsCfg config.CORSConfig
	require.NoError(t, env.Parse(&corsCfg))
	corsCfg.AllowOrigins = "https://app.example.com"

	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      corsCfg,
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(pingModule{})

	preflight, err := http.NewRequest(http.MethodOptions, "/api/v1/ping", http.NoBody)
	require.NoError(t, err)
	preflight.Header.Set(fiber.HeaderOrigin, "https://app.example.com")
	preflight.Header.Set(fiber.HeaderAccessControlRequestMethod, http.MethodPost)
	preflight.Header.Set(fiber.HeaderAccessControlRequestHeaders, "Idempotency-Key")

	resp, err := application.Fiber().Test(preflight)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Contains(t, resp.Header.Get(fiber.HeaderAccessControlAllowHeaders), "Idempotency-Key")

	actual, err := http.NewRequest(http.MethodGet, "/api/v1/ping", http.NoBody)
	require.NoError(t, err)
	actual.Header.Set(fiber.HeaderOrigin, "https://app.example.com")
	resp, err = application.Fiber().Test(actual)
	require.NoError(t, err)
	assert.True(t,
		strings.Contains(resp.Header.Get(fiber.HeaderAccessControlExposeHeaders), "Idempotent-Replayed"),
		"browser clients must be able to read the replay marker: %q",
		resp.Header.Get(fiber.HeaderAccessControlExposeHeaders),
	)
}

type providerModule struct {
	events    int
	jobs      int
	schedules int
}

func (m *providerModule) Name() string { return "provider" }
func (m *providerModule) RegisterRoutes(v1 fiber.Router, _ app.Deps) {
	v1.Get("/provider", func(c fiber.Ctx) error { return c.SendString("ok") })
}
func (m *providerModule) RegisterEvents(bus *events.Bus) {
	m.events++
	_ = bus.SubscribeDurable("provider.changed", "refresh", func(context.Context, events.Event) error {
		return nil
	})
}
func (m *providerModule) RegisterJobs(registrar jobs.Registrar) {
	m.jobs++
	registrar.Register("provider.reindex", func(context.Context, json.RawMessage) error { return nil })
}
func (m *providerModule) RegisterSchedules(registry *scheduler.Registry) {
	m.schedules++
	_ = registry.Register(scheduler.Spec{
		Name:     "provider.heartbeat",
		Interval: time.Minute,
		Kind:     "provider.reindex",
	})
}

type recordingRegistrar struct {
	kinds map[string]struct{}
}

func (r *recordingRegistrar) Register(kind string, _ jobs.Handler) {
	if r.kinds == nil {
		r.kinds = make(map[string]struct{})
	}
	r.kinds[kind] = struct{}{}
}

func TestOptionalModuleProviders(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	registry := scheduler.NewRegistry()
	registrar := &recordingRegistrar{}
	module := &providerModule{}

	application := app.New(cfg, app.Deps{
		Events:    events.New(nil),
		Jobs:      registrar,
		Schedules: registry,
	})
	application.Register(module)

	assert.Equal(t, 1, module.events)
	assert.Equal(t, 1, module.jobs)
	assert.Equal(t, 1, module.schedules)
	assert.Equal(t, 1, registry.Len())
	assert.Contains(t, registrar.kinds, "provider.reindex")
	assert.Contains(t, registrar.kinds, "events.provider.changed.refresh")
}

func TestOptionalModuleProvidersAreNilSafe(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	module := &providerModule{}
	application := app.New(cfg, app.Deps{})
	require.NotPanics(t, func() { application.Register(module) })
	assert.Equal(t, 0, module.events)
	assert.Equal(t, 0, module.jobs)
	assert.Equal(t, 0, module.schedules)
}

// APP_ROLE=worker: providers register, but no /api route is mounted.
func TestRegisterBackgroundRegistersProvidersWithoutRoutes(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5, Role: config.RoleWorker},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	registry := scheduler.NewRegistry()
	registrar := &recordingRegistrar{}
	module := &providerModule{}

	application := app.New(cfg, app.Deps{
		Events:    events.New(nil),
		Jobs:      registrar,
		Schedules: registry,
		Health:    health.NewChecker(nil, nil),
	})
	application.RegisterBackground(module)

	assert.Equal(t, 1, module.jobs)
	assert.Equal(t, 1, module.schedules)
	assert.Contains(t, registrar.kinds, "provider.reindex")
	assert.Contains(t, registrar.kinds, "events.provider.changed.refresh")

	req, _ := http.NewRequest("GET", "/api/v1/provider", http.NoBody)
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "worker role must not mount API routes")

	req, _ = http.NewRequest("GET", "/healthz", http.NoBody)
	resp, err = application.Fiber().Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "worker role keeps the health endpoints")
}
