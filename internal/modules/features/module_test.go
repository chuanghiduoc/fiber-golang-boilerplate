package features

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// stubService records the service calls the HTTP layer makes.
type stubService struct {
	updates int
	deletes int
	lastKey string
	flag    featuresservice.Flag
}

func (s *stubService) ListFlags(context.Context, features.Subject) ([]featuresservice.Flag, error) {
	return []featuresservice.Flag{{Key: "file_uploads", Enabled: true}}, nil
}

func (s *stubService) ListEvaluated(context.Context, features.Subject) ([]featuresservice.EvaluatedFlag, error) {
	return []featuresservice.EvaluatedFlag{{Key: "file_uploads", Enabled: true}}, nil
}

func (s *stubService) GetFlag(_ context.Context, key string, _ features.Subject) (*featuresservice.Flag, error) {
	s.lastKey = key
	flag := s.flag
	flag.Key = key
	return &flag, nil
}

func (s *stubService) CreateFlag(_ context.Context, key string, _ featuresservice.UpsertFlagInput, _ features.Subject) (*featuresservice.Flag, error) {
	s.lastKey = key
	return &featuresservice.Flag{Key: key, Configured: true}, nil
}

func (s *stubService) UpsertFlag(_ context.Context, key string, _ featuresservice.UpsertFlagInput, _ features.Subject) (*featuresservice.Flag, error) {
	s.updates++
	s.lastKey = key
	return &featuresservice.Flag{Key: key, Configured: true}, nil
}

func (s *stubService) DeleteFlag(_ context.Context, key string) error {
	s.deletes++
	s.lastKey = key
	return nil
}

func (s *stubService) UpdateFlag(ctx context.Context, key string, in featuresservice.UpsertFlagInput, subject features.Subject, _ featuresservice.ExpectedVersion) (*featuresservice.Flag, error) {
	return s.UpsertFlag(ctx, key, in, subject)
}

func (s *stubService) DeleteFlagVersion(ctx context.Context, key string, _ featuresservice.ExpectedVersion) error {
	return s.DeleteFlag(ctx, key)
}

func (s *stubService) ListTargets(context.Context, string) ([]featuresservice.Target, error) {
	return []featuresservice.Target{}, nil
}

func (s *stubService) ListAllTargets(context.Context, string) ([]featuresservice.Target, error) {
	return []featuresservice.Target{}, nil
}

func (s *stubService) PutTarget(_ context.Context, _ string, _ featuresservice.TargetRef, _ bool) (*featuresservice.Target, error) {
	return &featuresservice.Target{ID: testID(1)}, nil
}

func (s *stubService) DeleteTarget(context.Context, string, featuresservice.TargetRef) error {
	return nil
}

func moduleTestConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		JWT:       config.JWTConfig{Secret: "module-test-secret"},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
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

func (r *intentRecorder) RecordID(_ context.Context, event audit.Event) (audit.IntentRef, error) {
	r.intents = append(r.intents, event)
	return audit.IntentRef{ID: testID(len(r.intents))}, nil
}

func (r *intentRecorder) Complete(_ context.Context, _ audit.IntentRef, completion audit.Completion) error {
	r.completions = append(r.completions, completion)
	return nil
}

// mfaSessionID marks the session bearerMFA tokens belong to; stubSessions
// reports a fresh second factor only for it.
var mfaSessionID = testID(99)

// stubSessions is the session store RequireMFA needs. User 2 is a platform
// admin with the feature permissions; every other user has none.
type stubSessions struct{}

func (stubSessions) Validate(_ context.Context, claims session.Claims) (session.State, error) {
	state := session.State{TenantID: claims.TenantID, Active: true}
	if claims.UserID == testID(2) {
		state.Role = authz.RolePlatformAdmin
		state.Permissions = []string{
			string(featurescatalog.PermissionAdminFeaturesRead),
			string(featurescatalog.PermissionAdminFeaturesWrite),
			string(authz.PermissionPlatformFeaturesRead),
			string(authz.PermissionPlatformFeaturesWrite),
		}
		state.Platform = true
	}
	if claims.SessionID == mfaSessionID {
		state.MFAVerifiedAt = time.Now()
	}
	return state, nil
}

func (stubSessions) Invalidate(context.Context, uuid.UUID) error { return nil }

func (stubSessions) InvalidatePermissions(context.Context) error { return nil }

func setupModuleApp(t *testing.T, recorder audit.Recorder) (*fiber.App, *stubService) {
	t.Helper()
	svc := &stubService{flag: featuresservice.Flag{Configured: true, Enabled: true, UpdatedAt: timeUnix(1700000000)}}
	module := New(featureshandler.NewHandler(svc))
	catalogs, err := app.BuildCatalogs(module)
	if err != nil {
		t.Fatalf("BuildCatalogs: %v", err)
	}
	application := app.New(moduleTestConfig(), app.Deps{Cache: cache.NewMemoryCache(), Audit: recorder, Sessions: stubSessions{}, Catalogs: catalogs})
	module.RegisterRoutes(application.HTTP(), application.Deps())
	return application.Fiber(), svc
}

func timeUnix(seconds int64) *time.Time {
	t := time.Unix(seconds, 0).UTC()
	return &t
}

func bearer(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	tok, err := token.GenerateWithTenant(userID, testID(1), "module-test-secret", 24*time.Hour)
	require.NoError(t, err)
	return "Bearer " + tok
}

// bearerMFA mints a token that passed a phishing-resistant second factor.
// Platform routes require a passkey (hwk/swk) even for a platform admin.
func bearerMFA(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	return bearerAMR(t, userID, "pwd", "hwk")
}

func bearerAMR(t *testing.T, userID uuid.UUID, amr ...string) string {
	t.Helper()
	tok, err := token.GenerateForSession(token.SessionClaims{
		UserID: userID, TenantID: testID(1), SessionID: mfaSessionID,
		AMR: amr,
	}, "module-test-secret", 24*time.Hour)
	require.NoError(t, err)
	return "Bearer " + tok
}

func doRequest(t *testing.T, application *fiber.App, method, path, bearer string, body []byte, headers map[string]string) (resp *http.Response, payload []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err = application.Test(req)
	require.NoError(t, err)
	payload, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, payload
}

func TestModuleRoutesRequireFeaturePermissions(t *testing.T) {
	application, _ := setupModuleApp(t, &intentRecorder{})

	resp, _ := doRequest(t, application, http.MethodGet, "/api/v1/feature-flags", "", nil, nil)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	resp, _ = doRequest(t, application, http.MethodGet, "/api/v1/feature-flags", bearer(t, testID(1)), nil, nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	// Tenant routes do not require a second factor.
	resp, _ = doRequest(t, application, http.MethodGet, "/api/v1/feature-flags", bearer(t, testID(2)), nil, nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// TestPlatformRoutesRequireMFA pins the platform rule: platform:features:*
// forces RequireMFA on the read and write routes alike.
func TestPlatformRoutesRequireMFA(t *testing.T) {
	application, svc := setupModuleApp(t, &intentRecorder{})

	resp, payload := doRequest(t, application, http.MethodGet, "/api/v1/platform/feature-flags", bearer(t, testID(2)), nil, nil)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(payload), string(apperror.CodeMFARequired))

	resp, payload = doRequest(t, application, http.MethodGet, "/api/v1/platform/feature-flags", bearerAMR(t, testID(2), "pwd", "otp"), nil, nil)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode, "TOTP does not satisfy the platform passkey rule")
	assert.Contains(t, string(payload), string(apperror.CodeMFARequired))

	resp, _ = doRequest(t, application, http.MethodGet, "/api/v1/platform/feature-flags", bearerMFA(t, testID(2)), nil, nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	etag := `"1700000000000000000"`
	resp, _ = doRequest(t, application, http.MethodPatch, "/api/v1/platform/feature-flags/file_uploads",
		bearer(t, testID(2)), []byte(`{"enabled":false}`), map[string]string{"If-Match": etag})
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Zero(t, svc.updates, "an unverified session must not reach the service")

	resp, _ = doRequest(t, application, http.MethodPatch, "/api/v1/platform/feature-flags/file_uploads",
		bearerMFA(t, testID(2)), []byte(`{"enabled":false}`),
		map[string]string{"If-Match": etag, "Content-Type": "application/merge-patch+json"})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, svc.updates)
}

func TestModuleMutationRoutesAreIdempotent(t *testing.T) {
	application, svc := setupModuleApp(t, &intentRecorder{})
	body := []byte(`{"enabled":false}`)
	etag := `"1700000000000000000"`

	for i := 0; i < 2; i++ {
		resp, payload := doRequest(t, application, http.MethodPatch, "/api/v1/platform/feature-flags/file_uploads",
			bearerMFA(t, testID(2)), body, map[string]string{
				"Idempotency-Key": "feature-flag-update-1",
				"If-Match":        etag,
				"Content-Type":    "application/merge-patch+json",
			})
		require.Equalf(t, fiber.StatusOK, resp.StatusCode, "response body: %s", payload)
		if i == 1 {
			assert.Equal(t, "true", resp.Header.Get("Idempotent-Replayed"))
		}
	}
	assert.Equal(t, 1, svc.updates, "a replayed PATCH must not call the service twice")
}

func TestModuleMutationsFailClosedWithoutAuditStore(t *testing.T) {
	application, svc := setupModuleApp(t, nil)

	resp, _ := doRequest(t, application, http.MethodDelete, "/api/v1/platform/feature-flags/file_uploads",
		bearerMFA(t, testID(2)), nil, map[string]string{"If-Match": `"1700000000000000000"`})
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.Zero(t, svc.deletes, "an unaudited mutation must not reach the service")
}

func TestModuleDeleteFlagRecordsGuardIntent(t *testing.T) {
	recorder := &intentRecorder{}
	application, svc := setupModuleApp(t, recorder)

	resp, _ := doRequest(t, application, http.MethodDelete, "/api/v1/platform/feature-flags/file_uploads",
		bearerMFA(t, testID(2)), nil, map[string]string{"If-Match": `"1700000000000000000"`})
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Equal(t, 1, svc.deletes)
	require.Len(t, recorder.intents, 1)
	assert.Equal(t, "admin.feature_flag.deleted", recorder.intents[0].Action)
	assert.Equal(t, "feature_flag", recorder.intents[0].Resource)
	require.Len(t, recorder.completions, 1)
	assert.Equal(t, audit.OutcomeSucceeded, recorder.completions[0].Outcome)
}

func TestModuleCreateFlagReturnsLocation(t *testing.T) {
	application, _ := setupModuleApp(t, &intentRecorder{})

	resp, payload := doRequest(t, application, http.MethodPost, "/api/v1/platform/feature-flags",
		bearerMFA(t, testID(2)), []byte(`{"key":"file_uploads","killSwitch":false,"description":"uploads"}`), nil)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode)
	assert.Equal(t, "/api/v1/platform/feature-flags/file_uploads", resp.Header.Get("Location"))
	var flag map[string]any
	require.NoError(t, json.Unmarshal(payload, &flag))
	assert.Equal(t, "file_uploads", flag["key"])
}

func TestTargetRoutesRequireTheMappedPermissions(t *testing.T) {
	application, _ := setupModuleApp(t, &intentRecorder{})

	// A member without admin:features:write cannot change tenant targets.
	resp, _ := doRequest(t, application, http.MethodPut,
		"/api/v1/feature-flags/file_uploads/targets/tenant",
		bearer(t, testID(1)), []byte(`{"enabled":true}`), nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	// The platform target route accepts platform:features:write, so it also
	// requires a second factor.
	resp, _ = doRequest(t, application, http.MethodPut,
		"/api/v1/platform/feature-flags/file_uploads/targets/tenant:"+testID(3).String(),
		bearer(t, testID(2)), []byte(`{"enabled":true}`), nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	resp, _ = doRequest(t, application, http.MethodPut,
		"/api/v1/platform/feature-flags/file_uploads/targets/tenant:"+testID(3).String(),
		bearerMFA(t, testID(2)), []byte(`{"enabled":true}`), nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	// The tenant admin reaches the tenant route with admin:features:write.
	resp, _ = doRequest(t, application, http.MethodPut,
		"/api/v1/feature-flags/file_uploads/targets/user:"+testID(42).String(),
		bearer(t, testID(2)), []byte(`{"enabled":false}`), nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}
