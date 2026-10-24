package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/versioning"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func versioningTestConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

type versionedModule struct{}

func (versionedModule) Name() string { return "versioned" }
func (versionedModule) RegisterRoutes(v1 fiber.Router, _ app.Deps) {
	v1.Get("/shape", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"object": "ping", "version": "v1"})
	})
}
func (versionedModule) RegisterV2Routes(v2 fiber.Router, _ app.Deps) {
	v2.Get("/shape", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"object": "ping", "version": "v2"})
	})
}

type localizedModule struct{}

func (localizedModule) Name() string { return "localized" }
func (localizedModule) RegisterRoutes(v1 fiber.Router, _ app.Deps) {
	v1.Get("/localized", func(c fiber.Ctx) error {
		return apperror.NewCode(apperror.CodeUserNotFound, "fallback detail").
			WithDetailKey("errors.api.version_unsupported.detail", map[string]any{"version": "v9"})
	})
	v1.Get("/validate", func(c fiber.Ctx) error {
		return apperror.NewValidation("fallback detail", []apperror.FieldError{{
			Path:    "name",
			Code:    "too_short",
			Message: "name must be at least 3 characters",
			Params:  map[string]string{"min": "3"},
		}})
	})
}

func doAppRequest(t *testing.T, application *app.App, path string, headers map[string]string) (resp *http.Response, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	require.NoError(t, err)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err = application.Fiber().Test(req)
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), "body: %s", raw)
	return body
}

func TestVersionedModuleMountsV1AndV2(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(versionedModule{})

	resp, raw := doAppRequest(t, application, "/api/v1/shape", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", decodeMap(t, raw)["version"])
	assert.Equal(t, "v1", resp.Header.Get(versioning.ResponseHeader))
	assert.Contains(t, resp.Header.Get(fiber.HeaderVary), versioning.Header)

	resp, raw = doAppRequest(t, application, "/api/v2/shape", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, "v2", decodeMap(t, raw)["version"])
	assert.Equal(t, "v2", resp.Header.Get(versioning.ResponseHeader))
}

func TestMismatchedAcceptVersionIs406(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(versionedModule{})

	cases := []struct {
		path    string
		version string
	}{
		{"/api/v1/shape", "2"},
		{"/api/v1/shape", "v2"},
		{"/api/v2/shape", "1"},
		{"/api/v2/shape", "v1"},
	}
	for _, tt := range cases {
		resp, raw := doAppRequest(t, application, tt.path, map[string]string{versioning.Header: tt.version})
		require.Equal(t, fiber.StatusNotAcceptable, resp.StatusCode, "%s %s", tt.path, tt.version)
		body := decodeMap(t, raw)
		assert.Equal(t, string(apperror.CodeAPIVersionUnsupported), body["code"])
		assert.Equal(t, "/errors/api-version-unsupported", body["type"])
		assert.Equal(t, "application/problem+json", resp.Header.Get(fiber.HeaderContentType))
	}
}

func TestUnknownVersionPathIs406(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(versionedModule{})

	for _, path := range []string{"/api/v3/shape", "/api/v10/shape"} {
		resp, raw := doAppRequest(t, application, path, nil)
		require.Equal(t, fiber.StatusNotAcceptable, resp.StatusCode, path)
		body := decodeMap(t, raw)
		assert.Equal(t, string(apperror.CodeAPIVersionUnsupported), body["code"])
	}
}

func TestV2GroupAbsentWithoutV2Module(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(pingModule{})

	resp, _ := doAppRequest(t, application, "/api/v1/ping", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	resp, raw := doAppRequest(t, application, "/api/v2/ping", nil)
	require.Equal(t, fiber.StatusNotAcceptable, resp.StatusCode)
	assert.Equal(t, string(apperror.CodeAPIVersionUnsupported), decodeMap(t, raw)["code"])
}

func TestV1ResponseShapeUnchanged(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(versionedModule{})

	resp, raw := doAppRequest(t, application, "/api/v1/shape", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get(fiber.HeaderContentType), "application/json"))
	body := decodeMap(t, raw)
	assert.Equal(t, "ping", body["object"])
	assert.Equal(t, "v1", body["version"])
	assert.Len(t, body, 2)
}

func TestNilVersioningConfigHasNoDeprecationHeaders(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(pingModule{})

	resp, _ := doAppRequest(t, application, "/api/v1/ping", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Deprecation"))
	assert.Empty(t, resp.Header.Get("Sunset"))
	assert.Empty(t, resp.Header.Get("Link"))
	assert.Equal(t, "v1", resp.Header.Get(versioning.ResponseHeader))
}

func TestDeprecatedVersionHeaders(t *testing.T) {
	deprecatedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	sunsetAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	application := app.New(versioningTestConfig(), app.Deps{
		Versioning: &versioning.Config{
			Default: versioning.V1,
			Policies: map[versioning.Version]versioning.Policy{
				versioning.V1: {
					DeprecatedAt: deprecatedAt,
					SunsetAt:     sunsetAt,
					Successor:    versioning.V2,
					DocsURL:      "https://docs.example.com/deprecations/v1",
				},
			},
		},
	})
	application.Register(versionedModule{})

	resp, _ := doAppRequest(t, application, "/api/v1/shape", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Deprecation"))
	assert.Equal(t, sunsetAt.Format(http.TimeFormat), resp.Header.Get("Sunset"))
	assert.Contains(t, resp.Header.Get("Link"), "https://docs.example.com/deprecations/v1")
	assert.Contains(t, resp.Header.Get("Link"), "</api/v2>")
}

func TestI18nLocalizesErrorsWithoutChangingContract(t *testing.T) {
	bundle, err := i18n.New("en")
	require.NoError(t, err)
	application := app.New(versioningTestConfig(), app.Deps{I18n: bundle})
	application.Register(localizedModule{})

	resp, raw := doAppRequest(t, application, "/api/v1/localized",
		map[string]string{fiber.HeaderAcceptLanguage: "vi-VN, en;q=0.8"})
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "vi", resp.Header.Get(fiber.HeaderContentLanguage))
	assert.Contains(t, resp.Header.Get(fiber.HeaderVary), fiber.HeaderAcceptLanguage)
	assert.Contains(t, resp.Header.Get(fiber.HeaderVary), versioning.Header)
	body := decodeMap(t, raw)
	assert.Equal(t, "Không tìm thấy người dùng", body["title"])
	assert.Equal(t, "Phiên bản API v9 không được hỗ trợ.", body["detail"])
	assert.Equal(t, string(apperror.CodeUserNotFound), body["code"])
	assert.Equal(t, "/errors/user-not-found", body["type"])

	resp, raw = doAppRequest(t, application, "/api/v1/validate",
		map[string]string{fiber.HeaderAcceptLanguage: "vi"})
	require.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	body = decodeMap(t, raw)
	assert.Equal(t, "Dữ liệu không hợp lệ", body["title"])
	fields := body["errors"].([]any)
	assert.Equal(t, "Trường name phải có ít nhất 3 ký tự.", fields[0].(map[string]any)["message"])
	assert.Equal(t, "name", fields[0].(map[string]any)["path"])
	assert.Equal(t, "too_short", fields[0].(map[string]any)["code"])
	assert.NotContains(t, fields[0], "params")
}

func TestI18nFallbackLocale(t *testing.T) {
	bundle, err := i18n.New("en")
	require.NoError(t, err)
	application := app.New(versioningTestConfig(), app.Deps{I18n: bundle})
	application.Register(localizedModule{})

	resp, raw := doAppRequest(t, application, "/api/v1/localized",
		map[string]string{fiber.HeaderAcceptLanguage: "fr-FR"})
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "en", resp.Header.Get(fiber.HeaderContentLanguage))
	body := decodeMap(t, raw)
	assert.Equal(t, "User not found", body["title"])
	assert.Equal(t, "API version v9 is not supported.", body["detail"])
}

func TestNilI18nPreservesEnglishBehavior(t *testing.T) {
	application := app.New(versioningTestConfig(), app.Deps{})
	application.Register(localizedModule{})

	resp, raw := doAppRequest(t, application, "/api/v1/localized",
		map[string]string{fiber.HeaderAcceptLanguage: "vi"})
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	assert.Empty(t, resp.Header.Get(fiber.HeaderContentLanguage))
	body := decodeMap(t, raw)
	assert.Equal(t, "User not found", body["title"])
	assert.Equal(t, "fallback detail", body["detail"])
	assert.NotContains(t, resp.Header.Get(fiber.HeaderVary), fiber.HeaderAcceptLanguage)
}
