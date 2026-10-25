package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

type fakeService struct {
	flags        []featuresdto.FlagResponse
	detail       featuresdto.FlagDetailResponse
	targets      []featuresdto.TargetResponse
	upsertFlag   featuresdto.FlagResponse
	upsertTarget featuresdto.TargetResponse
	deleteErr    error
	lastSubject  features.Subject
	lastRequest  featuresdto.UpsertFlagRequest
}

func (f *fakeService) ListFlags(_ context.Context, subject features.Subject) ([]featuresdto.FlagResponse, error) {
	f.lastSubject = subject
	return f.flags, nil
}

func (f *fakeService) GetFlag(_ context.Context, _ string, subject features.Subject) (*featuresdto.FlagDetailResponse, error) {
	f.lastSubject = subject
	return &f.detail, nil
}

func (f *fakeService) UpsertFlag(_ context.Context, _ string, req featuresdto.UpsertFlagRequest, subject features.Subject) (*featuresdto.FlagResponse, error) {
	f.lastRequest = req
	f.lastSubject = subject
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &f.upsertFlag, nil
}

func (f *fakeService) DeleteFlag(context.Context, string) error {
	return f.deleteErr
}

func (f *fakeService) ListTargets(context.Context, string) ([]featuresdto.TargetResponse, error) {
	return f.targets, nil
}

func (f *fakeService) UpsertTarget(_ context.Context, _ string, _ featuresdto.UpsertTargetRequest) (*featuresdto.TargetResponse, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &f.upsertTarget, nil
}

func (f *fakeService) DeleteTarget(context.Context, string, int64) error {
	return f.deleteErr
}

func int64Ptr(value int64) *int64 { return &value }

func setupFeatureHandler(t *testing.T) (*fiber.App, *fakeService) {
	t.Helper()
	svc := &fakeService{
		flags: []featuresdto.FlagResponse{{Key: "file_uploads", Enabled: true, Effective: true, Source: "compiled_default"}},
		detail: featuresdto.FlagDetailResponse{
			Flag:    featuresdto.FlagResponse{Key: "file_uploads", Enabled: true},
			Targets: []featuresdto.TargetResponse{{ID: 1, FlagKey: "file_uploads", TenantID: 2, Enabled: false}},
		},
		targets:      []featuresdto.TargetResponse{{ID: 1, FlagKey: "file_uploads", TenantID: 2, Enabled: false}},
		upsertFlag:   featuresdto.FlagResponse{Key: "file_uploads", Enabled: false, KillSwitch: true, Configured: true, Source: "kill_switch"},
		upsertTarget: featuresdto.TargetResponse{ID: 5, FlagKey: "file_uploads", TenantID: 2, UserID: int64Ptr(42), Enabled: true},
	}
	h := NewHandler(svc)

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 88)
		c.SetContext(tenant.WithID(c.Context(), 2))
		return c.Next()
	})
	app.Get("/admin/features/flags", h.ListFlags)
	app.Get("/admin/features/flags/:key", h.GetFlag)
	app.Put("/admin/features/flags/:key", h.UpsertFlag)
	app.Delete("/admin/features/flags/:key", h.DeleteFlag)
	app.Get("/admin/features/flags/:key/targets", h.ListTargets)
	app.Put("/admin/features/flags/:key/targets", h.UpsertTarget)
	app.Delete("/admin/features/flags/:key/targets/:id", h.DeleteTarget)
	return app, svc
}

func doRequest(t *testing.T, app *fiber.App, method, path string, body []byte) (resp *http.Response, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err = app.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func TestHandlerListAndGetFlags(t *testing.T) {
	app, svc := setupFeatureHandler(t)

	resp, body := doRequest(t, app, http.MethodGet, "/admin/features/flags", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var list struct {
		Object string                     `json:"object"`
		Data   []featuresdto.FlagResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	assert.Equal(t, "list", list.Object)
	require.Len(t, list.Data, 1)
	assert.Equal(t, int64(88), svc.lastSubject.UserID)
	assert.Equal(t, int64(2), svc.lastSubject.TenantID)

	resp, body = doRequest(t, app, http.MethodGet, "/admin/features/flags/file_uploads", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var detail featuresdto.FlagDetailResponse
	require.NoError(t, json.Unmarshal(body, &detail))
	assert.Equal(t, "file_uploads", detail.Flag.Key)
	require.Len(t, detail.Targets, 1)

	resp, _ = doRequest(t, app, http.MethodGet, "/admin/features/flags/file_uploads/targets", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestHandlerUpsertFlag(t *testing.T) {
	app, svc := setupFeatureHandler(t)
	body, _ := json.Marshal(map[string]any{
		"enabled":     false,
		"killSwitch":  true,
		"description": "emergency off",
	})

	resp, respBody := doRequest(t, app, http.MethodPut, "/admin/features/flags/file_uploads", body)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var flag featuresdto.FlagResponse
	require.NoError(t, json.Unmarshal(respBody, &flag))
	assert.True(t, flag.KillSwitch)
	require.NotNil(t, svc.lastRequest.Enabled)
	assert.False(t, *svc.lastRequest.Enabled)

	// enabled is optional: omitting it clears the database default.
	resp, _ = doRequest(t, app, http.MethodPut, "/admin/features/flags/file_uploads", []byte(`{"killSwitch":false}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Nil(t, svc.lastRequest.Enabled)
}

func TestHandlerUpsertFlagPropagatesPlatformDenial(t *testing.T) {
	app, svc := setupFeatureHandler(t)
	svc.deleteErr = apperror.NewCode(apperror.CodePermissionDenied, "global feature flag changes require a platform tenant administrator")

	resp, body := doRequest(t, app, http.MethodPut, "/admin/features/flags/file_uploads", []byte(`{"enabled":true,"killSwitch":false}`))
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "permission.denied")
}

func TestHandlerUpsertFlagValidation(t *testing.T) {
	app, _ := setupFeatureHandler(t)

	resp, body := doRequest(t, app, http.MethodPut, "/admin/features/flags/file_uploads", []byte(`{"enabled":true}`))
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, string(body), "killSwitch")

	resp, _ = doRequest(t, app, http.MethodPut, "/admin/features/flags/file_uploads", []byte(`{"enabled":true,"killSwitch":false,"unknown":1}`))
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestHandlerUpsertAndDeleteTarget(t *testing.T) {
	app, _ := setupFeatureHandler(t)
	body, _ := json.Marshal(map[string]any{"userId": 42, "enabled": true})

	resp, respBody := doRequest(t, app, http.MethodPut, "/admin/features/flags/file_uploads/targets", body)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var target featuresdto.TargetResponse
	require.NoError(t, json.Unmarshal(respBody, &target))
	assert.Equal(t, int64(5), target.ID)

	resp, _ = doRequest(t, app, http.MethodDelete, "/admin/features/flags/file_uploads/targets/5", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	resp, _ = doRequest(t, app, http.MethodDelete, "/admin/features/flags/file_uploads/targets/not-a-number", nil)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestHandlerDeleteFlag(t *testing.T) {
	app, _ := setupFeatureHandler(t)

	resp, _ := doRequest(t, app, http.MethodDelete, "/admin/features/flags/file_uploads", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestHandlerDeleteFlagConflict(t *testing.T) {
	app, svc := setupFeatureHandler(t)
	svc.deleteErr = apperror.NewConflict("feature flag has targets; delete them before deleting the flag")

	resp, _ := doRequest(t, app, http.MethodDelete, "/admin/features/flags/file_uploads", nil)
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode)
}

func TestHandlerDeleteFlagNotFound(t *testing.T) {
	app, svc := setupFeatureHandler(t)
	svc.deleteErr = apperror.NewNotFound("feature flag not found")

	resp, _ := doRequest(t, app, http.MethodDelete, "/admin/features/flags/file_uploads", nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}
