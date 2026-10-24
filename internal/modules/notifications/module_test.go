package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

type routeService struct {
	markAllCalls int
	lastTenantID int64
	createCalls  int
}

func (s *routeService) Create(_ context.Context, input contracts.CreateNotificationInput) (service.CreateResult, error) {
	s.createCalls++
	now := time.Now().UTC()
	return service.CreateResult{
		Notification: &dto.NotificationResponse{
			ID: 1, TenantID: input.TenantID, UserID: input.UserID, Type: input.Type,
			Title: input.Title, Body: input.Body, CreatedAt: now,
		},
		EmailQueued: true,
	}, nil
}

func (s *routeService) Notify(context.Context, contracts.CreateNotificationInput) error { return nil }

func (s *routeService) Get(context.Context, int64, int64) (*dto.NotificationResponse, error) {
	return &dto.NotificationResponse{ID: 1, TenantID: 1, UserID: 10, Title: "One", Body: "Body"}, nil
}

func (s *routeService) List(ctx context.Context, _ int64, _ int, _ string, _ bool) ([]dto.NotificationResponse, bool, error) {
	s.lastTenantID = tenant.EffectiveID(ctx)
	return []dto.NotificationResponse{}, false, nil
}

func (s *routeService) MarkRead(context.Context, int64, int64) (*dto.NotificationResponse, error) {
	return &dto.NotificationResponse{ID: 1, TenantID: 1, UserID: 10, Title: "One", Body: "Body"}, nil
}

func (s *routeService) MarkAllRead(context.Context, int64) (int64, error) {
	s.markAllCalls++
	return 2, nil
}

func (s *routeService) UnreadCount(context.Context, int64) (int64, error) { return 3, nil }

func (s *routeService) ListPreferences(context.Context, int64) ([]dto.PreferenceResponse, error) {
	return []dto.PreferenceResponse{}, nil
}

func (s *routeService) SetPreference(_ context.Context, userID int64, req dto.UpdatePreferenceRequest) (*dto.PreferenceResponse, error) {
	return &dto.PreferenceResponse{ID: 1, TenantID: 1, UserID: userID, Type: req.Type, Channel: req.Channel, Enabled: req.Enabled}, nil
}

func newRouteApp(t *testing.T, svc *routeService) *fiber.App {
	t.Helper()
	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		JWT:       config.JWTConfig{Secret: "test-secret"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{Cache: cache.NewMemoryCache()})
	application.Register(New(notificationshandler.New(svc)))
	return application.Fiber()
}

func doRoute(t *testing.T, application *fiber.App, method, path, jwt string, body any) (resp *http.Response, payload []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err = application.Test(req)
	require.NoError(t, err)
	payload, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, payload
}

func TestModuleRequiresAuthenticationAndPermissions(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc)

	resp, _ := doRoute(t, application, http.MethodGet, "/api/v1/notifications", "", nil)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	userToken, err := token.GenerateWithTenant(10, 2, "user@example.com", "user", "test-secret", 24)
	require.NoError(t, err)
	resp, _ = doRoute(t, application, http.MethodGet, "/api/v1/notifications", userToken, nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(2), svc.lastTenantID, "service calls must inherit the JWT tenant")

	resp, _ = doRoute(t, application, http.MethodPost, "/api/v1/notifications/send", userToken, map[string]any{
		"userId": 11, "type": "system.info", "title": "Title", "body": "Body",
	})
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode, "users may not send notifications to others")
	assert.Zero(t, svc.createCalls)

	adminToken, err := token.GenerateWithTenant(1, 2, "admin@example.com", "admin", "test-secret", 24)
	require.NoError(t, err)
	resp, _ = doRoute(t, application, http.MethodPost, "/api/v1/notifications/send", adminToken, map[string]any{
		"userId": 11, "type": "system.info", "title": "Title", "body": "Body",
	})
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, svc.createCalls)
}

func TestModuleWriteRoutesAreIdempotent(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc)
	userToken, err := token.GenerateWithTenant(10, 1, "user@example.com", "user", "test-secret", 24)
	require.NoError(t, err)

	first, firstBody := doRouteWithKey(t, application, http.MethodPost, "/api/v1/notifications/read-all", userToken, "read-all-key")
	assert.Equal(t, fiber.StatusOK, first.StatusCode)
	assert.Equal(t, 1, svc.markAllCalls)

	second, secondBody := doRouteWithKey(t, application, http.MethodPost, "/api/v1/notifications/read-all", userToken, "read-all-key")
	assert.Equal(t, fiber.StatusOK, second.StatusCode)
	assert.Equal(t, "true", second.Header.Get("Idempotent-Replayed"))
	assert.Equal(t, 1, svc.markAllCalls, "the replay must not execute the mutation twice")
	assert.JSONEq(t, string(firstBody), string(secondBody))
}

func doRouteWithKey(t *testing.T, application *fiber.App, method, path, jwt, key string) (resp *http.Response, payload []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Idempotency-Key", key)
	resp, err = application.Test(req)
	require.NoError(t, err)
	payload, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, payload
}
