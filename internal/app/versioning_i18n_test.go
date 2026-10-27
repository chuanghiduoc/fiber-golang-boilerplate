package app_test

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func httpTestConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

type localizedModule struct{}

func (localizedModule) Name() string { return "localized" }

// localizedCode is registered through ErrorProvider and translated through
// LocaleProvider, exercising the module catalog path end to end.
const localizedCode apperror.Code = "localized.user_missing"

func (localizedModule) Errors() []apperror.Spec {
	return []apperror.Spec{{Code: localizedCode, Status: fiber.StatusNotFound, Title: "User not found", DocPath: "/errors/localized-user-missing"}}
}

func (localizedModule) Locales() fs.FS {
	return fstest.MapFS{
		"en.json": {Data: []byte(`{"errors.localized.user_missing.title": "User not found", "errors.localized.detail": "API version {version} is not supported."}`)},
		"vi.json": {Data: []byte(`{"errors.localized.user_missing.title": "Không tìm thấy người dùng", "errors.localized.detail": "Phiên bản API {version} không được hỗ trợ."}`)},
	}
}

func (localizedModule) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/localized", OperationID: "getLocalized",
		Tags: []string{"test"}, Summary: "Localized error",
	}, httpapi.Policy{Public: true}, func(_ context.Context, _ *struct{}) (*struct{}, error) {
		return nil, apperror.NewCode(localizedCode, "fallback detail").
			WithDetailKey("errors.localized.detail", map[string]any{"version": "v9"})
	})
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/validate", OperationID: "getValidate",
		Tags: []string{"test"}, Summary: "Validation error",
	}, httpapi.Policy{Public: true}, func(_ context.Context, _ *struct{}) (*struct{}, error) {
		return nil, apperror.NewValidation("fallback detail", []apperror.FieldError{{
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

func TestI18nLocalizesErrorsWithoutChangingContract(t *testing.T) {
	bundle, err := i18n.New("en", app.LocaleSources(localizedModule{})...)
	require.NoError(t, err)
	catalogs, err := app.BuildCatalogs(localizedModule{})
	require.NoError(t, err)
	application := app.New(httpTestConfig(), app.Deps{Sessions: allowAllSessions{}, I18n: bundle, Catalogs: catalogs})
	application.Register(localizedModule{})

	resp, raw := doAppRequest(t, application, "/api/v1/localized",
		map[string]string{fiber.HeaderAcceptLanguage: "vi-VN, en;q=0.8"})
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "vi", resp.Header.Get(fiber.HeaderContentLanguage))
	assert.Contains(t, resp.Header.Get(fiber.HeaderVary), fiber.HeaderAcceptLanguage)
	body := decodeMap(t, raw)
	assert.Equal(t, "Không tìm thấy người dùng", body["title"])
	assert.Equal(t, "Phiên bản API v9 không được hỗ trợ.", body["detail"])
	assert.Equal(t, string(localizedCode), body["code"])
	assert.Equal(t, "/errors/localized-user-missing", body["type"])
	assert.Equal(t, "application/problem+json", resp.Header.Get(fiber.HeaderContentType))

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
	bundle, err := i18n.New("en", app.LocaleSources(localizedModule{})...)
	require.NoError(t, err)
	catalogs, err := app.BuildCatalogs(localizedModule{})
	require.NoError(t, err)
	application := app.New(httpTestConfig(), app.Deps{Sessions: allowAllSessions{}, I18n: bundle, Catalogs: catalogs})
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
	catalogs, err := app.BuildCatalogs(localizedModule{})
	require.NoError(t, err)
	application := app.New(httpTestConfig(), app.Deps{Sessions: allowAllSessions{}, Catalogs: catalogs})
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
