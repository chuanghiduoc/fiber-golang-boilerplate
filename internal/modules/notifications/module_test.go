package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	notificationscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/catalog"
	notificationshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

type routeService struct {
	markAllCalls int
	lastTenantID uuid.UUID
	createCalls  int
	lastRead     bool
	lastUserID   uuid.UUID
	lastMemberID uuid.UUID
	err          error
}

func (s *routeService) Create(_ context.Context, input notificationcontracts.CreateNotificationInput) (service.CreateResult, error) {
	s.createCalls++
	s.lastMemberID = input.UserID
	now := time.Now().UTC()
	return service.CreateResult{
		Notification: &service.Notification{
			ID: testID(1), TenantID: input.TenantID, UserID: input.UserID, Type: input.Type,
			Title: input.Title, Body: input.Body, CreatedAt: now,
		},
		EmailQueued: true,
	}, nil
}

func (s *routeService) Notify(context.Context, notificationcontracts.CreateNotificationInput) error {
	return nil
}

func (s *routeService) Get(_ context.Context, userID, id uuid.UUID) (*service.Notification, error) {
	s.lastUserID = userID
	if s.err != nil {
		return nil, s.err
	}
	return &service.Notification{ID: id, TenantID: testID(1), UserID: userID, Title: "One", Body: "Body"}, nil
}

func (s *routeService) List(ctx context.Context, userID uuid.UUID, _ int, _ string, _ bool) ([]service.Notification, bool, error) {
	s.lastTenantID = tenant.IDFromContext(ctx)
	s.lastUserID = userID
	return []service.Notification{}, false, nil
}

func (s *routeService) SetRead(_ context.Context, userID, id uuid.UUID, read bool) (*service.Notification, error) {
	s.lastUserID = userID
	s.lastRead = read
	row := &service.Notification{ID: id, TenantID: testID(1), UserID: userID, Title: "One", Body: "Body"}
	if read {
		now := time.Now().UTC()
		row.ReadAt = &now
	}
	return row, nil
}

func (s *routeService) MarkRead(ctx context.Context, userID, id uuid.UUID) (*service.Notification, error) {
	return s.SetRead(ctx, userID, id, true)
}

func (s *routeService) MarkUnread(ctx context.Context, userID, id uuid.UUID) (*service.Notification, error) {
	return s.SetRead(ctx, userID, id, false)
}

func (s *routeService) MarkAllRead(context.Context, uuid.UUID) (int64, error) {
	s.markAllCalls++
	return 2, nil
}

func (s *routeService) UnreadCount(context.Context, uuid.UUID) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return 3, nil
}

func (s *routeService) ListPreferences(context.Context, uuid.UUID) ([]service.Preference, error) {
	return []service.Preference{}, nil
}

func (s *routeService) SetPreference(_ context.Context, userID uuid.UUID, update service.PreferenceUpdate) (*service.Preference, error) {
	return &service.Preference{TenantID: testID(1), UserID: userID, Type: update.Type, Channel: update.Channel, Enabled: *update.Enabled}, nil
}

// routeSessions is the session snapshot the auth middleware reads. User 1 is
// an admin with notifications:send; user 10 is a plain member. Both get the
// member-level notification permissions.
type routeSessions struct{}

func (routeSessions) Validate(_ context.Context, claims session.Claims) (session.State, error) {
	permissions := []string{
		string(notificationscatalog.PermissionNotificationsRead),
		string(notificationscatalog.PermissionNotificationsWrite),
	}
	role := authz.RoleMember
	if claims.UserID == testID(1) {
		role = authz.RoleAdmin
		permissions = append(permissions, string(notificationscatalog.PermissionNotificationsSend))
	}
	return session.State{Role: role, Permissions: permissions, Active: true, TenantActive: true}, nil
}

func (routeSessions) Invalidate(context.Context, uuid.UUID) error { return nil }

func (routeSessions) InvalidatePermissions(context.Context) error { return nil }

func moduleConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		JWT:       config.JWTConfig{Secret: "test-secret"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

func newRouteApp(t *testing.T, svc *routeService, recorder audit.Recorder) *fiber.App {
	t.Helper()
	module := New(notificationshandler.New(svc))
	catalogs, err := app.BuildCatalogs(module)
	if err != nil {
		t.Fatalf("BuildCatalogs: %v", err)
	}
	application := app.New(moduleConfig(), app.Deps{Cache: cache.NewMemoryCache(), Audit: recorder, Sessions: routeSessions{}, Catalogs: catalogs})
	module.RegisterRoutes(application.HTTP(), application.Deps())
	return application.Fiber()
}

// intentRecorder implements the Guard recorder contracts so the required
// audit guard on the member send can write intent rows in tests.
type intentRecorder struct {
	intents     []audit.Event
	completions []audit.Completion
}

func (r *intentRecorder) Record(context.Context, audit.Event) error { return nil }

func (r *intentRecorder) RecordID(_ context.Context, event audit.Event) (uuid.UUID, error) {
	r.intents = append(r.intents, event)
	return testID(len(r.intents)), nil
}

func (r *intentRecorder) Complete(_ context.Context, _ uuid.UUID, completion audit.Completion) error {
	r.completions = append(r.completions, completion)
	return nil
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

func doRouteWithKey(t *testing.T, application *fiber.App, method, path, jwt, key string, body any) (resp *http.Response, payload []byte) {
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
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Idempotency-Key", key)
	resp, err = application.Test(req)
	require.NoError(t, err)
	payload, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, payload
}

func memberToken(t *testing.T) string {
	t.Helper()
	userToken, err := token.GenerateWithTenant(testID(10), testID(2), "test-secret", 24*time.Hour)
	require.NoError(t, err)
	return userToken
}

func adminToken(t *testing.T) string {
	t.Helper()
	adminToken, err := token.GenerateWithTenant(testID(1), testID(2), "test-secret", 24*time.Hour)
	require.NoError(t, err)
	return adminToken
}

func TestModuleRequiresAuthenticationAndPermissions(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc, &intentRecorder{})

	resp, _ := doRoute(t, application, http.MethodGet, "/api/v1/me/notifications", "", nil)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	resp, _ = doRoute(t, application, http.MethodGet, "/api/v1/me/notifications", memberToken(t), nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, testID(2), svc.lastTenantID, "service calls must inherit the JWT tenant")

	resp, _ = doRoute(t, application, http.MethodPost, "/api/v1/members/"+testID(11).String()+"/notifications", memberToken(t), map[string]any{
		"type": "system.info", "title": "Title", "body": "Body",
	})
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode, "members may not send notifications to others")
	assert.Zero(t, svc.createCalls)

	admin := adminToken(t)
	resp, payload := doRoute(t, application, http.MethodPost, "/api/v1/members/"+testID(11).String()+"/notifications", admin, map[string]any{
		"type": "system.info", "title": "Title", "body": "Body",
	})
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
	assert.Equal(t, testID(11), svc.lastMemberID)
	assert.Equal(t, "/api/v1/me/notifications/"+testID(1).String(), resp.Header.Get("Location"))
	var created map[string]any
	require.NoError(t, json.Unmarshal(payload, &created))
	assert.Equal(t, true, created["emailQueued"])
}

func TestModuleSendFailsClosedWithoutAuditStore(t *testing.T) {
	svc := &routeService{}
	module := New(notificationshandler.New(svc))
	catalogs, err := app.BuildCatalogs(module)
	if err != nil {
		t.Fatalf("BuildCatalogs: %v", err)
	}
	application := app.New(moduleConfig(), app.Deps{Cache: cache.NewMemoryCache(), Sessions: routeSessions{}, Catalogs: catalogs})
	module.RegisterRoutes(application.HTTP(), application.Deps())

	resp, _ := doRoute(t, application.Fiber(), http.MethodPost, "/api/v1/members/"+testID(11).String()+"/notifications", adminToken(t), map[string]any{
		"type": "system.info", "title": "Title", "body": "Body",
	})
	assert.Equal(t, fiber.StatusServiceUnavailable, resp.StatusCode)
	assert.Zero(t, svc.createCalls, "an unaudited send must not reach the service")
}

func TestModuleWriteRoutesAreIdempotent(t *testing.T) {
	svc := &routeService{}
	recorder := &intentRecorder{}
	application := newRouteApp(t, svc, recorder)
	jwt := memberToken(t)

	first, firstBody := doRouteWithKey(t, application, http.MethodPatch, "/api/v1/me/notifications", jwt, "read-all-key",
		map[string]any{"read": true})
	assert.Equal(t, fiber.StatusOK, first.StatusCode)
	assert.Equal(t, 1, svc.markAllCalls)

	second, secondBody := doRouteWithKey(t, application, http.MethodPatch, "/api/v1/me/notifications", jwt, "read-all-key",
		map[string]any{"read": true})
	assert.Equal(t, fiber.StatusOK, second.StatusCode)
	assert.Equal(t, "true", second.Header.Get("Idempotent-Replayed"))
	assert.Equal(t, 1, svc.markAllCalls, "the replay must not execute the mutation twice")
	assert.JSONEq(t, string(firstBody), string(secondBody))
}

func TestModuleNotificationReadPatch(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc, &intentRecorder{})
	jwt := memberToken(t)
	path := "/api/v1/me/notifications/" + testID(3).String()

	resp, payload := doRoute(t, application, http.MethodPatch, path, jwt, map[string]any{"read": true})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.True(t, svc.lastRead)
	var updated map[string]any
	require.NoError(t, json.Unmarshal(payload, &updated))
	assert.Equal(t, testID(3).String(), updated["id"])
	assert.NotNil(t, updated["readAt"])
	assert.Equal(t, testID(10), svc.lastUserID)

	resp, payload = doRoute(t, application, http.MethodPatch, path, jwt, map[string]any{"read": false})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.False(t, svc.lastRead)
	var unread map[string]any
	require.NoError(t, json.Unmarshal(payload, &unread))
	_, hasReadAt := unread["readAt"]
	assert.False(t, hasReadAt, "read:false clears readAt")

	// An empty merge patch changes nothing and returns the current row.
	resp, _ = doRoute(t, application, http.MethodPatch, path, jwt, map[string]any{})
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	resp, _ = doRoute(t, application, http.MethodPatch, path, jwt, map[string]any{"read": "yes"})
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
}

func TestModuleBulkReadRejectsFalse(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc, &intentRecorder{})
	resp, _ := doRoute(t, application, http.MethodPatch, "/api/v1/me/notifications", memberToken(t),
		map[string]any{"read": false})
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
	assert.Zero(t, svc.markAllCalls)
}

func TestModulePreferenceItemURI(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc, &intentRecorder{})
	jwt := memberToken(t)

	resp, payload := doRoute(t, application, http.MethodGet, "/api/v1/me/notification-preferences", jwt, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var prefs struct {
		Preferences []map[string]any `json:"preferences"`
	}
	require.NoError(t, json.Unmarshal(payload, &prefs))
	assert.Empty(t, prefs.Preferences)

	resp, payload = doRoute(t, application, http.MethodPut,
		"/api/v1/me/notification-preferences/security.alert/email", jwt, map[string]any{"enabled": false})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var pref map[string]any
	require.NoError(t, json.Unmarshal(payload, &pref))
	assert.Equal(t, "security.alert", pref["type"])
	assert.Equal(t, "email", pref["channel"])
	assert.Equal(t, false, pref["enabled"])

	resp, _ = doRoute(t, application, http.MethodPut,
		"/api/v1/me/notification-preferences/security.alert/push", jwt, map[string]any{"enabled": true})
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)

	resp, _ = doRoute(t, application, http.MethodPut,
		"/api/v1/me/notification-preferences/security.alert/email", jwt, map[string]any{})
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
}

func TestModuleListEnvelopeAndUnreadCount(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc, &intentRecorder{})
	jwt := memberToken(t)

	resp, payload := doRoute(t, application, http.MethodGet, "/api/v1/me/notifications?limit=2&unreadOnly=true", jwt, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var list struct {
		Object string           `json:"object"`
		URL    string           `json:"url"`
		Data   []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(payload, &list))
	assert.Equal(t, "list", list.Object)
	assert.Equal(t, "/api/v1/me/notifications", list.URL)
	assert.Empty(t, list.Data)

	resp, payload = doRoute(t, application, http.MethodGet, "/api/v1/me/notifications/unread-count", jwt, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var count map[string]any
	require.NoError(t, json.Unmarshal(payload, &count))
	assert.Equal(t, float64(3), count["count"])
}

func TestModuleRejectsInvalidPathUUID(t *testing.T) {
	svc := &routeService{}
	application := newRouteApp(t, svc, &intentRecorder{})
	resp, _ := doRoute(t, application, http.MethodGet, "/api/v1/me/notifications/not-a-uuid", memberToken(t), nil)
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	assert.Zero(t, svc.lastUserID, "an invalid id must not reach the service")
}

func TestModulePropagatesServiceErrors(t *testing.T) {
	svc := &routeService{err: apperror.NewNotFound("notification not found")}
	application := newRouteApp(t, svc, &intentRecorder{})
	jwt := memberToken(t)

	resp, _ := doRoute(t, application, http.MethodGet, "/api/v1/me/notifications/"+testID(9).String(), jwt, nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	svc.err = errors.New("boom")
	resp, payload := doRoute(t, application, http.MethodGet, "/api/v1/me/notifications/unread-count", jwt, nil)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(payload, &problem))
	assert.Equal(t, string(apperror.CodeSystemInternal), problem.Code)
}
