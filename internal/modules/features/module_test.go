package features

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

type stubService struct {
	upserts int
}

func (s *stubService) ListFlags(context.Context, features.Subject) ([]featuresdto.FlagResponse, error) {
	return []featuresdto.FlagResponse{{Key: "file_uploads", Enabled: true}}, nil
}

func (s *stubService) GetFlag(context.Context, string, features.Subject) (*featuresdto.FlagDetailResponse, error) {
	return &featuresdto.FlagDetailResponse{Flag: featuresdto.FlagResponse{Key: "file_uploads"}}, nil
}

func (s *stubService) UpsertFlag(context.Context, string, featuresdto.UpsertFlagRequest, features.Subject) (*featuresdto.FlagResponse, error) {
	s.upserts++
	return &featuresdto.FlagResponse{Key: "file_uploads", Configured: true}, nil
}

func (s *stubService) DeleteFlag(context.Context, string) error { return nil }

func (s *stubService) ListTargets(context.Context, string) ([]featuresdto.TargetResponse, error) {
	return []featuresdto.TargetResponse{}, nil
}

func (s *stubService) UpsertTarget(context.Context, string, featuresdto.UpsertTargetRequest) (*featuresdto.TargetResponse, error) {
	return &featuresdto.TargetResponse{ID: 1}, nil
}

func (s *stubService) DeleteTarget(context.Context, string, int64) error { return nil }

func moduleTestConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		JWT:       config.JWTConfig{Secret: "module-test-secret"},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

// intentRecorder implements the Guard recorder contracts so required-mode
// routes can write intent rows in tests.
type intentRecorder struct {
	intents     []audit.Event
	completions []audit.Completion
}

func (r *intentRecorder) Record(context.Context, audit.Event) error { return nil }

func (r *intentRecorder) RecordID(_ context.Context, event audit.Event) (int64, error) {
	r.intents = append(r.intents, event)
	return int64(len(r.intents)), nil
}

func (r *intentRecorder) Complete(_ context.Context, _ int64, completion audit.Completion) error {
	r.completions = append(r.completions, completion)
	return nil
}

func setupModuleApp(t *testing.T) (*fiber.App, *stubService, *intentRecorder) {
	t.Helper()
	svc := &stubService{}
	recorder := &intentRecorder{}
	application := app.New(moduleTestConfig(), app.Deps{Cache: cache.NewMemoryCache(), Audit: recorder})
	application.Register(New(featureshandler.NewHandler(svc)))
	return application.Fiber(), svc, recorder
}

func bearer(t *testing.T, userID int64, role string) string {
	t.Helper()
	tok, err := token.GenerateWithTenant(userID, 1, "user@example.com", role, "module-test-secret", 24)
	require.NoError(t, err)
	return "Bearer " + tok
}

func TestModuleRoutesRequireFeaturePermissions(t *testing.T) {
	application, _, _ := setupModuleApp(t)

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/admin/features/flags", http.NoBody)
	resp, err := application.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	req, _ = http.NewRequest(http.MethodGet, "/api/v1/admin/features/flags", http.NoBody)
	req.Header.Set("Authorization", bearer(t, 1, "user"))
	resp, err = application.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	req, _ = http.NewRequest(http.MethodGet, "/api/v1/admin/features/flags", http.NoBody)
	req.Header.Set("Authorization", bearer(t, 2, "admin"))
	resp, err = application.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestModuleMutationRoutesAreIdempotent(t *testing.T) {
	application, svc, recorder := setupModuleApp(t)
	body := []byte(`{"enabled":true,"killSwitch":false}`)

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPut, "/api/v1/admin/features/flags/file_uploads", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", bearer(t, 2, "admin"))
		req.Header.Set("Idempotency-Key", "feature-flag-update-1")
		resp, err := application.Test(req)
		require.NoError(t, err)
		assert.Equal(t, fiber.StatusOK, resp.StatusCode)
		if i == 1 {
			assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
		}
	}
	assert.Equal(t, 1, svc.upserts, "replayed PUT must not call the service twice")
	require.Len(t, recorder.intents, 1, "replay must not write a second intent row")
	assert.Equal(t, "admin.feature_flag.updated", recorder.intents[0].Action)
	assert.Equal(t, "feature_flag", recorder.intents[0].Resource)
	require.Len(t, recorder.completions, 1)
	assert.Equal(t, audit.OutcomeSucceeded, recorder.completions[0].Outcome)
}

func TestModuleMutationsFailClosedWithoutAuditStore(t *testing.T) {
	svc := &stubService{}
	// No Deps.Audit: the required Guard has no intent-capable recorder.
	application := app.New(moduleTestConfig(), app.Deps{Cache: cache.NewMemoryCache()})
	application.Register(New(featureshandler.NewHandler(svc)))

	req, _ := http.NewRequest(http.MethodPut, "/api/v1/admin/features/flags/file_uploads",
		bytes.NewReader([]byte(`{"enabled":true,"killSwitch":false}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearer(t, 2, "admin"))
	resp, err := application.Fiber().Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, 0, svc.upserts, "an unaudited mutation must not reach the service")
}
