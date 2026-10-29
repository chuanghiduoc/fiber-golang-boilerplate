package handler

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

	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuresservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

type fakeService struct {
	flags       []featuresservice.Flag
	flag        featuresservice.Flag
	evaluated   []featuresservice.EvaluatedFlag
	targets     []featuresservice.Target
	created     featuresservice.Flag
	updated     featuresservice.Flag
	upserted    featuresservice.Target
	err         error
	lastSubject features.Subject
	lastCreate  string
	lastUpdate  featuresservice.UpsertFlagInput
	lastRef     featuresservice.TargetRef
	lastEnabled bool
	lastDelete  string
	lastDelRef  featuresservice.TargetRef
	upserts     int
	lastVersion featuresservice.ExpectedVersion
}

func (f *fakeService) ListFlags(_ context.Context, subject features.Subject) ([]featuresservice.Flag, error) {
	f.lastSubject = subject
	return f.flags, f.err
}

func (f *fakeService) ListEvaluated(_ context.Context, subject features.Subject) ([]featuresservice.EvaluatedFlag, error) {
	f.lastSubject = subject
	return f.evaluated, f.err
}

func (f *fakeService) GetFlag(_ context.Context, key string, subject features.Subject) (*featuresservice.Flag, error) {
	f.lastSubject = subject
	f.lastCreate = key
	if f.err != nil {
		return nil, f.err
	}
	flag := f.flag
	flag.Key = key
	return &flag, nil
}

func (f *fakeService) CreateFlag(_ context.Context, key string, in featuresservice.UpsertFlagInput, subject features.Subject) (*featuresservice.Flag, error) {
	f.lastCreate = key
	f.lastUpdate = in
	f.lastSubject = subject
	if f.err != nil {
		return nil, f.err
	}
	flag := f.created
	flag.Key = key
	return &flag, nil
}

func (f *fakeService) UpsertFlag(_ context.Context, key string, in featuresservice.UpsertFlagInput, subject features.Subject) (*featuresservice.Flag, error) {
	f.upserts++
	f.lastCreate = key
	f.lastUpdate = in
	f.lastSubject = subject
	if f.err != nil {
		return nil, f.err
	}
	flag := f.updated
	flag.Key = key
	return &flag, nil
}

func (f *fakeService) UpdateFlag(ctx context.Context, key string, in featuresservice.UpsertFlagInput, subject features.Subject, version featuresservice.ExpectedVersion) (*featuresservice.Flag, error) {
	f.lastVersion = version
	return f.UpsertFlag(ctx, key, in, subject)
}

func (f *fakeService) DeleteFlagVersion(ctx context.Context, key string, version featuresservice.ExpectedVersion) error {
	f.lastVersion = version
	return f.DeleteFlag(ctx, key)
}

func (f *fakeService) DeleteFlag(_ context.Context, key string) error {
	f.lastDelete = key
	return f.err
}

func (f *fakeService) ListTargets(context.Context, string) ([]featuresservice.Target, error) {
	return f.targets, f.err
}

func (f *fakeService) ListAllTargets(context.Context, string) ([]featuresservice.Target, error) {
	return f.targets, f.err
}

func (f *fakeService) PutTarget(_ context.Context, _ string, ref featuresservice.TargetRef, enabled bool) (*featuresservice.Target, error) {
	f.lastRef = ref
	f.lastEnabled = enabled
	if f.err != nil {
		return nil, f.err
	}
	target := f.upserted
	return &target, nil
}

func (f *fakeService) DeleteTarget(_ context.Context, key string, ref featuresservice.TargetRef) error {
	f.lastDelete = key
	f.lastDelRef = ref
	return f.err
}

// intentRecorder implements Guard's intent and completion contracts so the
// required-mode mutations reach the handlers.
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

func uuidPtr(value uuid.UUID) *uuid.UUID { return &value }

// setupFeatureHandler builds the huma API the way internal/app does and mounts
// only this module's operations behind a stub authenticated platform admin.
func setupFeatureHandler(t *testing.T) (*fiber.App, *fakeService, *intentRecorder) {
	t.Helper()
	svc := &fakeService{
		flags: []featuresservice.Flag{{Key: "file_uploads", Enabled: true, Effective: true, Source: "compiled_default", KillSwitch: true}},
		flag: featuresservice.Flag{
			Key: "file_uploads", Enabled: true, Configured: true, Effective: true,
			Source: "database_default", KillSwitch: true, Description: "uploads",
			UpdatedAt: timePtr(time.Unix(1700000000, 0)),
		},
		evaluated: []featuresservice.EvaluatedFlag{{Key: "file_uploads", Enabled: true, Source: "compiled_default"}},
		targets: []featuresservice.Target{
			{ID: testID(1), FlagKey: "file_uploads", TenantID: testID(2), Enabled: false, UserID: uuidPtr(testID(42))},
			{ID: testID(2), FlagKey: "file_uploads", TenantID: testID(3), Enabled: true},
		},
		created:  featuresservice.Flag{Enabled: true, Configured: true, KillSwitch: false, UpdatedAt: timePtr(time.Unix(1700000100, 0))},
		updated:  featuresservice.Flag{Enabled: false, Configured: true, KillSwitch: true, UpdatedAt: timePtr(time.Unix(1700000200, 0))},
		upserted: featuresservice.Target{ID: testID(5), FlagKey: "file_uploads", TenantID: testID(2), UserID: uuidPtr(testID(42)), Enabled: true},
	}
	recorder := &intentRecorder{}

	fapp := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := httpapi.New(fapp, fapp, httpapi.DefaultConfig("feature test", "1.0.0", "test API"), httpapi.Deps{
		Authenticate:    stubAuth,
		AuthenticateJWT: stubAuth,
		Idempotency:     func(c fiber.Ctx) error { return c.Next() },
		Audit:           recorder,
	})
	NewHandler(svc).Register(api)
	return fapp, svc, recorder
}

// stubAuth installs a fresh platform admin for tenant 2 with a second factor.
func stubAuth(c fiber.Ctx) error {
	c.SetContext(tenant.WithID(c.Context(), testID(2)))
	subject := authz.NewSubjectWithPermissions(testID(88), testID(2), authz.RolePlatformAdmin,
		featurescatalog.PermissionAdminFeaturesRead,
		featurescatalog.PermissionAdminFeaturesWrite,
		authz.PermissionPlatformFeaturesRead,
		authz.PermissionPlatformFeaturesWrite,
	).WithAMR([]string{"pwd", "hwk"}).WithMFAVerifiedAt(time.Now())
	authzhttp.SetSubject(c, subject)
	return c.Next()
}

func timePtr(value time.Time) *time.Time { return &value }

func doRequest(t *testing.T, app *fiber.App, method, path string, body []byte, headers map[string]string) (resp *http.Response, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err = app.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func TestHandlerListAndGetFlags(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)

	resp, body := doRequest(t, app, http.MethodGet, platformFlagsPath, nil, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, string(body))
	var list struct {
		Object string                            `json:"object"`
		URL    string                            `json:"url"`
		Data   []featuresdto.FeatureFlagResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	assert.Equal(t, "list", list.Object)
	assert.Equal(t, platformFlagsPath, list.URL)
	require.Len(t, list.Data, 1)
	assert.Equal(t, testID(88), svc.lastSubject.UserID)
	assert.Equal(t, testID(2), svc.lastSubject.TenantID)

	resp, body = doRequest(t, app, http.MethodGet, platformFlagsPath+"/file_uploads", nil, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, `"1700000000000000000"`, resp.Header.Get("ETag"))
	var flag featuresdto.FeatureFlagResponse
	require.NoError(t, json.Unmarshal(body, &flag))
	assert.Equal(t, "file_uploads", flag.Key)
	assert.True(t, flag.KillSwitch)
}

func TestHandlerCreateFlag(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)

	resp, body := doRequest(t, app, http.MethodPost, platformFlagsPath,
		[]byte(`{"key":"file_uploads","enabled":true,"killSwitch":false,"description":"uploads"}`), nil)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode)
	assert.Equal(t, platformFlagsPath+"/file_uploads", resp.Header.Get("Location"))
	var flag featuresdto.FeatureFlagResponse
	require.NoError(t, json.Unmarshal(body, &flag))
	assert.Equal(t, "file_uploads", flag.Key)
	assert.Equal(t, "file_uploads", svc.lastCreate)
	require.NotNil(t, svc.lastUpdate.Enabled)
	assert.True(t, *svc.lastUpdate.Enabled)

	svc.err = apperror.NewConflict("feature flag already exists; update it instead")
	resp, _ = doRequest(t, app, http.MethodPost, platformFlagsPath,
		[]byte(`{"key":"file_uploads","killSwitch":false,"description":"uploads"}`), nil)
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode)

	svc.err = nil
	resp, _ = doRequest(t, app, http.MethodPost, platformFlagsPath, []byte(`{"key":"file_uploads","description":"uploads"}`), nil)
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode, "killSwitch is required")
}

func TestHandlerUpdateFlagMergePatch(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)
	etag := `"1700000000000000000"`

	resp, body := doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{"enabled":null,"killSwitch":true}`),
		map[string]string{"Content-Type": "application/merge-patch+json", "If-Match": etag})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var flag featuresdto.FeatureFlagResponse
	require.NoError(t, json.Unmarshal(body, &flag))
	assert.Equal(t, "file_uploads", flag.Key)
	assert.Equal(t, `"1700000200000000000"`, resp.Header.Get("ETag"), "the response carries the new validator")

	assert.Nil(t, svc.lastUpdate.Enabled, "an explicit null clears the database default")
	require.NotNil(t, svc.lastUpdate.KillSwitch)
	assert.True(t, *svc.lastUpdate.KillSwitch)
	require.NotNil(t, svc.lastUpdate.Description)
	assert.Equal(t, "uploads", *svc.lastUpdate.Description, "an absent member is unchanged")
	assert.True(t, svc.lastVersion.Exists, "the write carries the read version")
	assert.Equal(t, time.Unix(1700000000, 0).UTC(), svc.lastVersion.UpdatedAt.UTC())

	resp, _ = doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{"enabled":true}`), map[string]string{
			"Content-Type": "application/merge-patch+json",
			"If-Match":     `"stale"`,
		})
	assert.Equal(t, fiber.StatusPreconditionFailed, resp.StatusCode)

	resp, _ = doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{"enabled":true}`), map[string]string{"Content-Type": "application/merge-patch+json"})
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "a missing If-Match is unconditional")
	require.NotNil(t, svc.lastUpdate.Enabled)
	assert.True(t, *svc.lastUpdate.Enabled)

	// A concurrent change between the read and the write surfaces as the same
	// 412 the stale-validator check returns.
	svc.err = apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
	resp, _ = doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{"enabled":true}`), map[string]string{"Content-Type": "application/merge-patch+json", "If-Match": etag})
	assert.Equal(t, fiber.StatusPreconditionFailed, resp.StatusCode)
	svc.err = nil
}

func TestHandlerDeleteFlag(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)

	resp, _ := doRequest(t, app, http.MethodDelete, platformFlagsPath+"/file_uploads", nil,
		map[string]string{"If-Match": `"1700000000000000000"`})
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "file_uploads", svc.lastDelete)

	resp, _ = doRequest(t, app, http.MethodDelete, platformFlagsPath+"/file_uploads", nil,
		map[string]string{"If-Match": `"stale"`})
	assert.Equal(t, fiber.StatusPreconditionFailed, resp.StatusCode)

	svc.err = apperror.NewConflict("feature flag has targets; delete them before deleting the flag")
	resp, _ = doRequest(t, app, http.MethodDelete, platformFlagsPath+"/file_uploads", nil, nil)
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode)

	svc.err = apperror.NewNotFound("feature flag not found")
	resp, _ = doRequest(t, app, http.MethodDelete, platformFlagsPath+"/file_uploads", nil, nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestHandlerPlatformTargets(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)

	resp, body := doRequest(t, app, http.MethodGet, platformFlagsPath+"/file_uploads/targets", nil, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var list struct {
		Data []featuresdto.FeatureFlagTargetResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Data, 2)
	assert.Equal(t, "user:"+testID(2).String()+":"+testID(42).String(), list.Data[0].TargetID)
	assert.Equal(t, "tenant:"+testID(3).String(), list.Data[1].TargetID)

	resp, body = doRequest(t, app, http.MethodPut, platformFlagsPath+"/file_uploads/targets/user:"+testID(2).String()+":"+testID(42).String(),
		[]byte(`{"enabled":true}`), nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.NotNil(t, svc.lastRef.TenantID)
	assert.Equal(t, testID(2), *svc.lastRef.TenantID)
	require.NotNil(t, svc.lastRef.UserID)
	assert.Equal(t, testID(42), *svc.lastRef.UserID)
	assert.True(t, svc.lastEnabled)
	var target featuresdto.FeatureFlagTargetResponse
	require.NoError(t, json.Unmarshal(body, &target))
	assert.Equal(t, "user:"+testID(2).String()+":"+testID(42).String(), target.TargetID)

	resp, _ = doRequest(t, app, http.MethodPut, platformFlagsPath+"/file_uploads/targets/tenant:"+testID(3).String(),
		[]byte(`{"enabled":false}`), nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.NotNil(t, svc.lastRef.TenantID)
	assert.Equal(t, testID(3), *svc.lastRef.TenantID)
	assert.Nil(t, svc.lastRef.UserID)

	resp, _ = doRequest(t, app, http.MethodDelete, platformFlagsPath+"/file_uploads/targets/user:"+testID(2).String()+":"+testID(42).String(), nil, nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	require.NotNil(t, svc.lastDelRef.UserID)

	for _, invalid := range []string{"file_uploads", "tenant:not-a-uuid", "user:" + testID(2).String()} {
		resp, _ = doRequest(t, app, http.MethodDelete, platformFlagsPath+"/file_uploads/targets/"+invalid, nil, nil)
		assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode, "targetId %q", invalid)
	}
}

func TestHandlerTenantTargets(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)

	resp, body := doRequest(t, app, http.MethodGet, tenantFlagsPath, nil, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var list struct {
		Data []featuresdto.EvaluatedFlagResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Data, 1)
	assert.Equal(t, "file_uploads", list.Data[0].Key)
	assert.True(t, list.Data[0].Enabled)

	resp, body = doRequest(t, app, http.MethodGet, tenantFlagsPath+"/file_uploads/targets", nil, nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var tenantList struct {
		Data []featuresdto.FeatureFlagTargetResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &tenantList))
	require.Len(t, tenantList.Data, 2)
	assert.Equal(t, "user:"+testID(42).String(), tenantList.Data[0].TargetID)
	assert.Equal(t, "tenant", tenantList.Data[1].TargetID)

	resp, _ = doRequest(t, app, http.MethodPut, tenantFlagsPath+"/file_uploads/targets/tenant",
		[]byte(`{"enabled":false}`), nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Nil(t, svc.lastRef.TenantID)
	assert.Nil(t, svc.lastRef.UserID)

	resp, _ = doRequest(t, app, http.MethodPut, tenantFlagsPath+"/file_uploads/targets/user:"+testID(42).String(),
		[]byte(`{"enabled":true}`), nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.NotNil(t, svc.lastRef.UserID)
	assert.Equal(t, testID(42), *svc.lastRef.UserID)
	assert.Nil(t, svc.lastRef.TenantID)

	resp, _ = doRequest(t, app, http.MethodDelete, tenantFlagsPath+"/file_uploads/targets/tenant", nil, nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Nil(t, svc.lastDelRef.UserID)
}

func TestHandlerPropagatesServiceErrors(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)
	svc.err = apperror.NewCode(apperror.CodePermissionDenied, "global feature flag changes require a platform administrator")
	resp, body := doRequest(t, app, http.MethodGet, platformFlagsPath, nil, nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "permission.denied")
}

func TestHandlerEmptyMergePatchIsANoOp(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)
	etag := `"1700000000000000000"`

	resp, body := doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{}`), map[string]string{"Content-Type": "application/merge-patch+json", "If-Match": etag})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Zero(t, svc.upserts, "an empty merge patch must not write")
	assert.Equal(t, etag, resp.Header.Get("ETag"))

	var flag map[string]any
	require.NoError(t, json.Unmarshal(body, &flag))
	assert.Equal(t, "file_uploads", flag["key"])
}

func TestHandlerDescriptionNullAndAbsent(t *testing.T) {
	app, svc, _ := setupFeatureHandler(t)

	resp, _ := doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{"description":null}`), map[string]string{"Content-Type": "application/merge-patch+json"})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.NotNil(t, svc.lastUpdate.Description)
	assert.Equal(t, "", *svc.lastUpdate.Description, "null clears the custom description")

	resp, _ = doRequest(t, app, http.MethodPatch, platformFlagsPath+"/file_uploads",
		[]byte(`{"enabled":true}`), map[string]string{"Content-Type": "application/merge-patch+json"})
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.NotNil(t, svc.lastUpdate.Description)
	assert.Equal(t, "uploads", *svc.lastUpdate.Description, "an absent member keeps the current description")
}

func TestHandlerTargetListURLIsConcrete(t *testing.T) {
	app, _, _ := setupFeatureHandler(t)

	_, body := doRequest(t, app, http.MethodGet, platformFlagsPath+"/file_uploads/targets", nil, nil)
	var list struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	assert.Equal(t, "/api/v1/platform/feature-flags/file_uploads/targets", list.URL)

	_, body = doRequest(t, app, http.MethodGet, tenantFlagsPath+"/file_uploads/targets", nil, nil)
	require.NoError(t, json.Unmarshal(body, &list))
	assert.Equal(t, "/api/v1/feature-flags/file_uploads/targets", list.URL)
}

func TestInvalidTargetIDMessageMatchesParserContract(t *testing.T) {
	_, err := parsePlatformTargetID("user:not-a-uuid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported or invalid format")
	assert.NotContains(t, err.Error(), "user:{userId},")
}
