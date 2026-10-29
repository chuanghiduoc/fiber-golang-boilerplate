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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

type pingModule struct{}

func (pingModule) Name() string { return "ping" }
func (pingModule) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/ping", OperationID: "getPing",
		Tags: []string{"test"}, Summary: "Ping",
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*pingOutput, error) {
		return &pingOutput{Body: "pong"}, nil
	})
}

type pingOutput struct {
	Body string
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

func (m flagModule) RegisterRoutes(api *httpapi.API, deps app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/flag", OperationID: "getFlag",
		Tags: []string{"test"}, Summary: "Flag",
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*flagOutput, error) {
		return &flagOutput{Body: m.enabled(deps)}, nil
	})
}

type flagOutput struct {
	Body bool
}

type idempotentModule struct {
	calls *int
}

func (idempotentModule) Name() string { return "idempotent" }

func (m idempotentModule) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/idem", OperationID: "createIdem",
		Tags: []string{"test"}, Summary: "Idempotent write",
	}, httpapi.Policy{Public: true, Idempotent: true, NoStore: true}, func(context.Context, *struct{}) (*pingOutput, error) {
		(*m.calls)++
		return &pingOutput{Body: "created"}, nil
	})
}

func TestHumaOperationReplaysIdempotentResponse(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	calls := 0
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}, Cache: cache.NewMemoryCache()})
	application.Register(idempotentModule{calls: &calls})

	first, err := http.NewRequest(http.MethodPost, "/api/v1/idem", http.NoBody)
	require.NoError(t, err)
	first.Header.Set("Idempotency-Key", "app-idem-1")
	resp, err := application.Fiber().Test(first)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get(fiber.HeaderCacheControl))
	firstBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	second, err := http.NewRequest(http.MethodPost, "/api/v1/idem", http.NoBody)
	require.NoError(t, err)
	second.Header.Set("Idempotency-Key", "app-idem-1")
	resp, err = application.Fiber().Test(second)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
	assert.Equal(t, "no-store", resp.Header.Get(fiber.HeaderCacheControl), "replays must keep no-store")
	secondBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, string(firstBody), string(secondBody))
	assert.Equal(t, 1, calls, "the replay must not run the handler twice")
}

type staticTenantResolver struct{}

func (staticTenantResolver) Resolve(_ context.Context, slug string) (tenant.Tenant, error) {
	if slug == "default" {
		return tenant.Tenant{ID: tenant.DefaultID, Slug: "default", Name: "Default"}, nil
	}
	return tenant.Tenant{}, apperror.ErrNotFound
}

// TestMetricsEndpointExposesSemconvHTTPServerHistogram scrapes the real
// infrastructure route: the removed request counter must be gone and the OTel
// HTTP server-duration histogram must carry the semantic-convention labels.
func TestMetricsEndpointExposesSemconvHTTPServerHistogram(t *testing.T) {
	provider, err := metrics.Setup(metrics.Options{ServiceVersion: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})
	application.Register(pingModule{})

	ping, _ := http.NewRequest(http.MethodGet, "/api/v1/ping", http.NoBody)
	resp, err := application.Fiber().Test(ping)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	scrape, _ := http.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	resp, err = application.Fiber().Test(scrape)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	text := string(body)

	assert.Contains(t, text, "# TYPE http_server_request_duration_seconds histogram")
	assert.Contains(t, text, `http_request_method="GET"`)
	assert.Contains(t, text, `http_route="/api/v1/ping"`)
	assert.Contains(t, text, `http_response_status_code="200"`)
	assert.NotContains(t, text, "http_requests_total")
}

func TestModuleRegistryMountsRoutes(t *testing.T) {
	cfg := &config.Config{
		// production disables the Swagger file loader, which resolves relative
		// to the process working directory (the package dir during go test).
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})
	application.Register(pingModule{})

	req, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAppProvidesNilSafeFeatureEvaluator(t *testing.T) {
	cfg := &config.Config{
		App:          config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:         config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit:    config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:       config.TenantConfig{DefaultSlug: "default"},
		FeatureFlags: config.FeatureFlagsConfig{Flags: []string{"app_test_flag=false"}},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})
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
	assert.Equal(t, "false", strings.TrimSpace(string(body)), "FEATURE_FLAGS must apply when Deps.Flags is nil")
}

// catalogModule exercises every catalog provider hook.
type catalogModule struct {
	name       string
	permission authz.Permission
	code       apperror.Code
	flag       features.Key
	locales    fstest.MapFS
}

func (m catalogModule) Name() string { return m.name }
func (m catalogModule) Permissions() []authz.Grant {
	return []authz.Grant{{Permission: m.permission, Roles: []string{authz.RoleMember}}}
}
func (m catalogModule) Errors() []apperror.Spec {
	return []apperror.Spec{{Code: m.code, Status: 409, Title: "Gadget busy", DocPath: "/errors/gadget-busy"}}
}
func (m catalogModule) FeatureFlags() []features.Definition {
	return []features.Definition{{Key: m.flag, Description: "gadget flag"}}
}
func (m catalogModule) Locales() fs.FS { return m.locales }

func TestBuildCatalogsWiresEveryProvider(t *testing.T) {
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
	catalogs, err := app.BuildCatalogs(module)
	require.NoError(t, err)

	assert.True(t, catalogs.Permissions.Has(authz.RoleMember, "gadgets:read"))
	assert.True(t, catalogs.Permissions.Has(authz.RoleAdmin, "gadgets:read"), "admins receive every permission")
	spec, ok := catalogs.Errors.SpecFor("gadget.busy")
	require.True(t, ok)
	assert.Equal(t, 409, spec.Status)
	assert.True(t, catalogs.Features.IsKnown("gadgets_beta"))

	bundle, err := i18n.New("en", app.LocaleSources(module)...)
	require.NoError(t, err)
	assert.Equal(t, "Thiết bị bận", bundle.Translate("vi", "errors.gadget.busy.title", nil))
}

func TestBuildCatalogsRejectsConflicts(t *testing.T) {
	owner := catalogModule{name: "owner_mod", permission: "ownermod:read", code: "ownermod.busy", flag: "ownermod_beta"}
	thief := owner
	thief.name = "thief_mod"
	err := app.NewCatalogs().Register(owner, thief)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already owned")

	err = app.NewCatalogs().Register(pingModule{}, pingModule{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered twice")
}

func TestTenantMiddlewareRejectsUnknownHeader(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}, TenantResolver: staticTenantResolver{}, Health: health.NewChecker(nil, nil)})
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
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default", RequireHeader: true},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}, TenantResolver: staticTenantResolver{}})
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
	corsCfg.AllowOrigins = []string{"https://app.example.com"}

	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      corsCfg,
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})
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
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"), "preflight must still be correlated")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), "preflight must carry security headers")

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

// CORS preflight is answered at the transport layer before routing. An unknown
// path therefore gets 204 rather than 404: this deliberately does not reveal
// whether a route exists, and no handler, auth check, or audit row runs.
func TestCORSPreflightUnknownPathIsHandledBeforeRouting(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"https://app.example.com"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})

	preflight, err := http.NewRequest(http.MethodOptions, "/api/v1/does-not-exist", http.NoBody)
	require.NoError(t, err)
	preflight.Header.Set(fiber.HeaderOrigin, "https://app.example.com")
	preflight.Header.Set(fiber.HeaderAccessControlRequestMethod, http.MethodGet)

	resp, err := application.Fiber().Test(preflight)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
}

// Credentialed CORS is enabled only by the refresh-cookie transport; JSON
// mode must never advertise credentials with a wildcard-capable client.
func TestCORSAllowCredentialsOnlyInCookieMode(t *testing.T) {
	for _, test := range []struct {
		name       string
		cookieMode bool
		wantCreds  string
	}{
		{name: "json mode", cookieMode: false, wantCreds: ""},
		{name: "cookie mode", cookieMode: true, wantCreds: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{
				App:  config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
				CORS: config.CORSConfig{AllowOrigins: []string{"https://app.example.com"}},
				JWT:  config.JWTConfig{RefreshCookie: test.cookieMode},
				RateLimit: config.RateLimitConfig{
					StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second,
					RelaxedMax: 120, RelaxedWindow: 60 * time.Second,
				},
			}
			application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})
			application.Register(pingModule{})

			req, err := http.NewRequest(http.MethodGet, "/api/v1/ping", http.NoBody)
			require.NoError(t, err)
			req.Header.Set(fiber.HeaderOrigin, "https://app.example.com")
			resp, err := application.Fiber().Test(req)
			require.NoError(t, err)
			assert.Equal(t, test.wantCreds, resp.Header.Get(fiber.HeaderAccessControlAllowCredentials))
		})
	}
}

type providerModule struct {
	events    int
	jobs      int
	schedules int
}

func (m *providerModule) Name() string { return "provider" }
func (m *providerModule) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/provider", OperationID: "getProvider",
		Tags: []string{"test"}, Summary: "Provider",
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*providerOutput, error) {
		return &providerOutput{Body: "ok"}, nil
	})
}

type providerOutput struct {
	Body string
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
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	registry := scheduler.NewRegistry()
	registrar := &recordingRegistrar{}
	module := &providerModule{}

	application := app.New(cfg, app.Deps{
		Sessions:  allowAllSessions{},
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
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	module := &providerModule{}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}})
	require.NotPanics(t, func() { application.Register(module) })
	assert.Equal(t, 0, module.events)
	assert.Equal(t, 0, module.jobs)
	assert.Equal(t, 0, module.schedules)
}

// APP_ROLE=worker: providers register, but no /api route is mounted.
func TestRegisterBackgroundRegistersProvidersWithoutRoutes(t *testing.T) {
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second, Role: config.RoleWorker},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	registry := scheduler.NewRegistry()
	registrar := &recordingRegistrar{}
	module := &providerModule{}

	application := app.New(cfg, app.Deps{
		Sessions:  allowAllSessions{},
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

// allowAllSessions is a permissive session store for tests that exercise the
// HTTP foundation rather than session validation.
type allowAllSessions struct{}

func (allowAllSessions) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{Active: true, TenantActive: true}, nil
}

func (allowAllSessions) Invalidate(context.Context, uuid.UUID) error { return nil }
func (allowAllSessions) InvalidatePermissions(context.Context) error { return nil }
