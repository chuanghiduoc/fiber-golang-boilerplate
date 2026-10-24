package handler

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeService struct {
	items          []dto.NotificationResponse
	prefs          []dto.PreferenceResponse
	unread         int64
	updated        int64
	hasMore        bool
	createResult   service.CreateResult
	err            error
	lastUserID     int64
	lastTenantID   int64
	lastLimit      int
	lastAfter      string
	lastUnreadOnly bool
	lastPreference dto.UpdatePreferenceRequest
	lastInput      notificationcontracts.CreateNotificationInput
}

func (f *fakeService) Create(_ context.Context, input notificationcontracts.CreateNotificationInput) (service.CreateResult, error) {
	f.lastInput = input
	return f.createResult, f.err
}

func (f *fakeService) Notify(_ context.Context, _ notificationcontracts.CreateNotificationInput) error {
	return f.err
}

func (f *fakeService) Get(_ context.Context, userID, id int64) (*dto.NotificationResponse, error) {
	f.lastUserID = userID
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.items {
		if f.items[i].ID == id {
			return &f.items[i], nil
		}
	}
	return nil, apperror.NewNotFound("notification not found")
}

func (f *fakeService) List(ctx context.Context, userID int64, limit int, startingAfter string, unreadOnly bool) ([]dto.NotificationResponse, bool, error) {
	f.lastUserID = userID
	f.lastTenantID = tenant.EffectiveID(ctx)
	f.lastLimit = limit
	f.lastAfter = startingAfter
	f.lastUnreadOnly = unreadOnly
	return f.items, f.hasMore, f.err
}

func (f *fakeService) MarkRead(_ context.Context, userID, id int64) (*dto.NotificationResponse, error) {
	f.lastUserID = userID
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.items {
		if f.items[i].ID == id {
			readAt := time.Now().UTC()
			f.items[i].ReadAt = &readAt
			return &f.items[i], nil
		}
	}
	return nil, apperror.NewNotFound("notification not found")
}

func (f *fakeService) MarkAllRead(_ context.Context, userID int64) (int64, error) {
	f.lastUserID = userID
	return f.updated, f.err
}

func (f *fakeService) UnreadCount(_ context.Context, userID int64) (int64, error) {
	f.lastUserID = userID
	return f.unread, f.err
}

func (f *fakeService) ListPreferences(_ context.Context, userID int64) ([]dto.PreferenceResponse, error) {
	f.lastUserID = userID
	return f.prefs, f.err
}

func (f *fakeService) SetPreference(_ context.Context, userID int64, req dto.UpdatePreferenceRequest) (*dto.PreferenceResponse, error) {
	f.lastUserID = userID
	f.lastPreference = req
	if f.err != nil {
		return nil, f.err
	}
	return &dto.PreferenceResponse{
		ID: 1, TenantID: 1, UserID: userID, Type: req.Type, Channel: req.Channel, Enabled: *req.Enabled,
	}, nil
}

func setupHandlerApp(h *Handler) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 10)
		fiber.Locals[string](c, "role", "user")
		c.SetContext(tenant.WithID(c.Context(), 1))
		return c.Next()
	})

	app.Get("/notifications/unread-count", h.UnreadCount)
	app.Get("/notifications", h.List)
	app.Get("/notifications/:id", h.Get)
	app.Post("/notifications/read-all", h.MarkAllRead)
	app.Post("/notifications/:id/read", h.MarkRead)
	app.Post("/notifications/send", h.Send)
	app.Get("/notification-preferences", h.ListPreferences)
	app.Put("/notification-preferences", h.SetPreference)
	return app
}

func doJSON(t *testing.T, app *fiber.App, method, path string, body any) (resp *http.Response, parsed map[string]any) {
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
	resp, err = app.Test(req)
	require.NoError(t, err)
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if len(payload) > 0 {
		require.NoError(t, json.Unmarshal(payload, &parsed))
	}
	return resp, parsed
}

func TestListHandler(t *testing.T) {
	now := time.Now().UTC()
	svc := &fakeService{hasMore: true, items: []dto.NotificationResponse{
		{ID: 2, TenantID: 1, UserID: 10, Type: "system.info", Title: "New", Body: "Body", CreatedAt: now},
		{ID: 1, TenantID: 1, UserID: 10, Type: "system.info", Title: "Old", Body: "Body", CreatedAt: now.Add(-time.Minute)},
	}}
	app := setupHandlerApp(New(svc))

	req, _ := http.NewRequest(http.MethodGet, "/notifications?limit=2&unreadOnly=true", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Object     string                     `json:"object"`
		URL        string                     `json:"url"`
		Data       []dto.NotificationResponse `json:"data"`
		HasMore    bool                       `json:"hasMore"`
		NextCursor string                     `json:"nextCursor"`
	}
	require.NoError(t, json.Unmarshal(body, &parsed))
	assert.Equal(t, "list", parsed.Object)
	assert.Equal(t, "/notifications", parsed.URL)
	require.Len(t, parsed.Data, 2)
	assert.Equal(t, int64(2), parsed.Data[0].ID)
	assert.NotEmpty(t, parsed.NextCursor)
	assert.Equal(t, int64(10), svc.lastUserID)
	assert.Equal(t, 2, svc.lastLimit)
	assert.True(t, svc.lastUnreadOnly)
}

func TestUnreadAndReadHandlers(t *testing.T) {
	now := time.Now().UTC()
	svc := &fakeService{
		items:   []dto.NotificationResponse{{ID: 1, TenantID: 1, UserID: 10, Title: "One", Body: "Body", CreatedAt: now}},
		unread:  3,
		updated: 4,
	}
	app := setupHandlerApp(New(svc))

	resp, body := doJSON(t, app, http.MethodGet, "/notifications/unread-count", nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, float64(3), body["count"])

	resp, body = doJSON(t, app, http.MethodPost, "/notifications/1/read", nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, float64(1), body["id"])
	assert.NotNil(t, body["readAt"])

	resp, body = doJSON(t, app, http.MethodPost, "/notifications/read-all", nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, float64(4), body["updated"])
}

func TestPreferenceHandlers(t *testing.T) {
	svc := &fakeService{prefs: []dto.PreferenceResponse{
		{ID: 1, TenantID: 1, UserID: 10, Type: "security.alert", Channel: "email", Enabled: false},
	}}
	app := setupHandlerApp(New(svc))

	resp, body := doJSON(t, app, http.MethodGet, "/notification-preferences", nil)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	data, ok := body["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 1)

	resp, body = doJSON(t, app, http.MethodPut, "/notification-preferences", dto.UpdatePreferenceRequest{
		Type: "security.alert", Channel: "email", Enabled: new(true),
	})
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, true, body["enabled"])
	assert.Equal(t, "security.alert", svc.lastPreference.Type)

	resp, _ = doJSON(t, app, http.MethodPut, "/notification-preferences", map[string]any{
		"type": "security.alert", "channel": "push", "enabled": true,
	})
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)

	// Omitting enabled is a validation error, not an implicit disable;
	// explicit false is accepted.
	svc.lastPreference = dto.UpdatePreferenceRequest{}
	resp, _ = doJSON(t, app, http.MethodPut, "/notification-preferences", map[string]any{
		"type": "security.alert", "channel": "email",
	})
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	assert.Empty(t, svc.lastPreference.Type, "the service must not be called")
	resp, body = doJSON(t, app, http.MethodPut, "/notification-preferences", map[string]any{
		"type": "security.alert", "channel": "email", "enabled": false,
	})
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, false, body["enabled"])

	resp, _ = doJSON(t, app, http.MethodPut, "/notification-preferences", map[string]any{
		"type": "security.alert", "channel": "email", "enabled": true, "unknown": 1,
	})
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestSendHandler(t *testing.T) {
	now := time.Now().UTC()
	svc := &fakeService{createResult: service.CreateResult{
		Notification: &dto.NotificationResponse{ID: 1, TenantID: 1, UserID: 10, Title: "Hello", Body: "Body", CreatedAt: now},
		EmailQueued:  true,
	}}
	app := setupHandlerApp(New(svc))

	resp, body := doJSON(t, app, http.MethodPost, "/notifications/send", map[string]any{
		"userId": 10,
		"type":   "system.info",
		"title":  "Hello",
		"body":   "Body",
		"data":   map[string]any{"k": "v"},
	})
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
	assert.Equal(t, true, body["emailQueued"])
	assert.Equal(t, int64(1), svc.lastInput.TenantID)
	assert.Equal(t, int64(10), svc.lastInput.UserID)
	assert.JSONEq(t, `{"k":"v"}`, string(svc.lastInput.Data))

	// In-app disabled: the documented contract is "notification": null, not
	// an absent field.
	svc.createResult = service.CreateResult{EmailQueued: true}
	resp, body = doJSON(t, app, http.MethodPost, "/notifications/send", map[string]any{
		"userId": 10, "type": "system.info", "title": "Hello", "body": "Body",
	})
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
	notification, present := body["notification"]
	assert.True(t, present, "notification must be present")
	assert.Nil(t, notification)
}

func TestHandlerPropagatesErrors(t *testing.T) {
	svc := &fakeService{err: apperror.NewNotFound("notification not found")}
	app := setupHandlerApp(New(svc))

	resp, _ := doJSON(t, app, http.MethodGet, "/notifications/9", nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	svc.err = errors.New("boom")
	resp, _ = doJSON(t, app, http.MethodGet, "/notifications/unread-count", nil)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
}
