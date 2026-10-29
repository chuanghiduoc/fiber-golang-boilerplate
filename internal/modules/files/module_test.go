package files

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const filesModuleSecret = "files-module-test-secret"

type moduleSessions struct{}

func (moduleSessions) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{
		Role:         authz.RoleMember,
		Permissions:  []string{string(filescatalog.PermissionFilesWrite)},
		Active:       true,
		TenantActive: true,
	}, nil
}

func (moduleSessions) Invalidate(context.Context, uuid.UUID) error { return nil }
func (moduleSessions) InvalidatePermissions(context.Context) error { return nil }

// moduleAudit satisfies the Guard's intent and completion contracts.
type moduleAudit struct{}

func (moduleAudit) Record(context.Context, audit.Event) error { return nil }
func (moduleAudit) RecordID(context.Context, audit.Event) (audit.IntentRef, error) {
	return audit.IntentRef{ID: uuid.New()}, nil
}
func (moduleAudit) Complete(context.Context, audit.IntentRef, audit.Completion) error { return nil }

// moduleUploadService is enough of the files service for the upload gate test.
type moduleUploadService struct{}

func (moduleUploadService) Upload(_ context.Context, userID uuid.UUID, filename string, _ io.Reader, size int64, contentType string) (*filesvc.File, error) {
	return &filesvc.File{
		ID: testID(2), TenantID: testID(1), UserID: userID,
		OriginalName: filename, MimeType: contentType, Size: size,
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}, nil
}

func (moduleUploadService) GetFileInfo(context.Context, uuid.UUID, uuid.UUID) (*filesvc.File, error) {
	return nil, apperror.NewNotFound("file not found")
}

func (moduleUploadService) Download(context.Context, uuid.UUID, uuid.UUID) (*filesvc.File, io.ReadCloser, error) {
	return nil, nil, apperror.NewNotFound("file not found")
}

func (moduleUploadService) List(context.Context, uuid.UUID, int, string, bool) ([]filesvc.File, bool, error) {
	return nil, false, nil
}

func (moduleUploadService) Delete(context.Context, uuid.UUID, uuid.UUID) error {
	return apperror.NewNotFound("file not found")
}

func (moduleUploadService) Restore(context.Context, uuid.UUID, uuid.UUID) (*filesvc.File, error) {
	return nil, apperror.NewNotFound("file not found")
}

func moduleConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "test", BodyLimit: 1 << 20, JSONBodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		JWT:       config.JWTConfig{Secret: filesModuleSecret},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 1000, StrictWindow: 60 * time.Second, NormalMax: 1000, NormalWindow: 60 * time.Second, RelaxedMax: 1000, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

func moduleApp(t *testing.T, svc filesvc.UploadService, flags features.Evaluator) (*app.App, *Module) {
	t.Helper()
	module := New(fileshandler.NewUploadHandler(svc, 10*1024*1024, []string{"text/plain; charset=utf-8"}))
	catalogs, err := app.BuildCatalogs(module)
	require.NoError(t, err)
	application := app.New(moduleConfig(), app.Deps{
		Cache:    cache.NewMemoryCache(),
		Sessions: moduleSessions{},
		Audit:    moduleAudit{},
		Flags:    flags,
		Catalogs: catalogs,
	})
	application.Register(module)
	return application, module
}

func filesFeatureCatalog() *features.Catalog {
	catalog := features.NewCatalog()
	if err := catalog.Add(features.Provider{Owner: filescatalog.Owner, Definitions: filescatalog.FeatureFlags()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func uploadRequest(t *testing.T) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "note.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello file"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req, err := http.NewRequest(http.MethodPost, "/api/v1/files", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	tok, err := token.GenerateWithTenant(testID(5), testID(1), filesModuleSecret, 24*time.Hour)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

func TestUploadGatedByFileUploadsFlag(t *testing.T) {
	disabled, _ := moduleApp(t, moduleUploadService{}, features.NewEvaluator(features.Options{
		Env: map[features.Key]bool{filescatalog.KeyFileUploads: false},
	}))
	resp, err := disabled.Fiber().Test(uploadRequest(t))
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, string(raw), string(apperror.CodeFeatureDisabled))

	enabled, _ := moduleApp(t, moduleUploadService{}, features.NewEvaluator(features.Options{Catalog: filesFeatureCatalog()}))
	resp, err = enabled.Fiber().Test(uploadRequest(t))
	require.NoError(t, err)
	raw, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "/api/v1/files/"+testID(2).String(), resp.Header.Get("Location"))
	var created dto.FileResponse
	require.NoError(t, json.Unmarshal(raw, &created))
	assert.Equal(t, testID(2), created.ID)
	assert.True(t, strings.HasSuffix(created.URL, "/content"))
}

func TestModuleRegistersCanonicalFileOperations(t *testing.T) {
	application, _ := moduleApp(t, moduleUploadService{}, nil)

	byID := map[string]httpapi.OperationInfo{}
	for _, op := range application.HTTP().Operations() {
		if strings.HasPrefix(op.Op.Path, "/api/v1/files") {
			byID[op.Op.OperationID] = op
		}
	}
	require.Len(t, byID, 6)

	cases := []struct {
		id         string
		method     string
		path       string
		permission authz.Permission
		tier       httpapi.RateLimitTier
		idempotent bool
		noStore    bool
		guard      string
	}{
		{"listFiles", http.MethodGet, "/api/v1/files", filescatalog.PermissionFilesRead, httpapi.RateLimitRelaxed, false, false, ""},
		{"createFile", http.MethodPost, "/api/v1/files", filescatalog.PermissionFilesWrite, httpapi.RateLimitNormal, true, false, "file.uploaded"},
		{"getFile", http.MethodGet, "/api/v1/files/{fileId}", filescatalog.PermissionFilesRead, httpapi.RateLimitRelaxed, false, false, ""},
		{"getFileContent", http.MethodGet, "/api/v1/files/{fileId}/content", filescatalog.PermissionFilesRead, httpapi.RateLimitRelaxed, false, true, ""},
		{"deleteFile", http.MethodDelete, "/api/v1/files/{fileId}", filescatalog.PermissionFilesDelete, httpapi.RateLimitNormal, true, false, "file.deleted"},
		{"restoreFile", http.MethodPost, "/api/v1/files/{fileId}/restore", filescatalog.PermissionFilesRestore, httpapi.RateLimitNormal, true, false, "file.restored"},
	}
	for _, tc := range cases {
		op, ok := byID[tc.id]
		require.True(t, ok, "missing operation %s", tc.id)
		assert.Equal(t, tc.method, op.Op.Method, tc.id)
		assert.Equal(t, tc.path, op.Op.Path, tc.id)
		assert.Equal(t, tc.permission, op.Policy.Permission, tc.id)
		assert.Equal(t, tc.tier, op.Policy.RateLimit, tc.id)
		assert.Equal(t, tc.idempotent, op.Policy.Idempotent, tc.id)
		assert.Equal(t, tc.noStore, op.Policy.NoStore, tc.id)
		if tc.guard == "" {
			assert.Nil(t, op.Policy.Audit.Guard, tc.id)
			continue
		}
		require.NotNil(t, op.Policy.Audit.Guard, tc.id)
		assert.Equal(t, tc.guard, op.Policy.Audit.Guard.Action, tc.id)
		assert.Equal(t, "file", op.Policy.Audit.Guard.Resource, tc.id)
	}
}
