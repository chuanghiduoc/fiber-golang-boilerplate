package app_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
)

type pingModule struct{}

func (pingModule) Name() string { return "ping" }
func (pingModule) RegisterRoutes(v1 fiber.Router, _ app.Deps) {
	v1.Get("/ping", func(c fiber.Ctx) error { return c.SendString("pong") })
}

func TestModuleRegistryMountsRoutes(t *testing.T) {
	cfg := &config.Config{
		// production disables the Swagger file loader, which resolves relative
		// to the process working directory (the package dir during go test).
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(pingModule{})

	req, _ := http.NewRequest("GET", "/api/v1/ping", http.NoBody)
	resp, err := application.Fiber().Test(req)
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
