package app_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
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
		FeatureFlags: config.FeatureFlagsConfig{Flags: "file_uploads=false"},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(flagModule{enabled: func(deps app.Deps) bool {
		if deps.Flags == nil {
			t.Fatal("app.New must install a default feature evaluator")
		}
		return deps.Flags.Enabled(context.Background(), features.KeyFileUploads, features.Subject{})
	}})

	req, _ := http.NewRequest("GET", "/api/v1/flag", http.NoBody)
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "false", string(body), "FEATURE_FLAGS must apply when Deps.Flags is nil")
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
