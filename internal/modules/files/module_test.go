package files

import (
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const filesModuleSecret = "files-module-test-secret"

func uploadStatus(t *testing.T, flags features.Evaluator) (status int, body string) {
	t.Helper()
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		JWT:       config.JWTConfig{Secret: filesModuleSecret},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{Cache: cache.NewMemoryCache(), Flags: flags})
	application.Register(New(fileshandler.NewUploadHandler(nil, 0, nil)))

	tok, err := token.GenerateWithTenant(5, 1, "user@example.com", "user", filesModuleSecret, 24)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/files/upload", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

func TestUploadGatedByFileUploadsFlag(t *testing.T) {
	status, body := uploadStatus(t, features.NewEvaluator(features.Options{
		Env: map[features.Key]bool{filescatalog.KeyFileUploads: false},
	}))
	assert.Equal(t, fiber.StatusNotFound, status)
	assert.Contains(t, body, string(apperror.CodeFeatureDisabled))

	// Enabled: the request passes the gate and reaches the required audit
	// guard, which fails closed here because no audit store is wired. This
	// also proves the gate runs before the guard writes an intent row.
	status, body = uploadStatus(t, features.NewEvaluator(features.Options{}))
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Contains(t, body, string(apperror.CodeSystemAuditUnavailable))
}
