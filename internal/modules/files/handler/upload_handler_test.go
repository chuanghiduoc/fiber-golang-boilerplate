package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	filesmodule "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const handlerTestSecret = "files-handler-test-secret"

func handlerTestID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

// mockUploadService is the handler's view of the files service.
type mockUploadService struct {
	files map[uuid.UUID]*filesvc.File
}

func newMockUploadService() *mockUploadService {
	return &mockUploadService{
		files: map[uuid.UUID]*filesvc.File{
			handlerTestID(1): {
				ID: handlerTestID(1), TenantID: handlerTestID(1), UserID: handlerTestID(1),
				OriginalName: "test.pdf", MimeType: "application/pdf", Size: 1024,
				CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			},
		},
	}
}

func (m *mockUploadService) Upload(_ context.Context, userID uuid.UUID, filename string, _ io.Reader, size int64, contentType string) (*filesvc.File, error) {
	return &filesvc.File{
		ID: handlerTestID(2), TenantID: handlerTestID(1), UserID: userID,
		OriginalName: filename, MimeType: contentType, Size: size,
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}, nil
}

func (m *mockUploadService) GetFileInfo(_ context.Context, id, _ uuid.UUID) (*filesvc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.NewNotFound("file not found")
	}
	return f, nil
}

func (m *mockUploadService) Download(_ context.Context, id, _ uuid.UUID) (*filesvc.File, io.ReadCloser, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, nil, apperror.NewNotFound("file not found")
	}
	return f, io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), int(f.Size)))), nil
}

func (m *mockUploadService) List(context.Context, uuid.UUID, int, string, bool) ([]filesvc.File, bool, error) {
	files := make([]filesvc.File, 0, len(m.files))
	for _, f := range m.files {
		files = append(files, *f)
	}
	return files, false, nil
}

func (m *mockUploadService) Delete(_ context.Context, id, _ uuid.UUID) error {
	if _, ok := m.files[id]; !ok {
		return apperror.NewNotFound("file not found")
	}
	delete(m.files, id)
	return nil
}

func (m *mockUploadService) Restore(_ context.Context, id, _ uuid.UUID) (*filesvc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.NewNotFound("file not found")
	}
	return f, nil
}

// handlerSessions grants the member file permissions from the session snapshot.
type handlerSessions struct{}

func (handlerSessions) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{
		Role: "member",
		Permissions: []string{
			string(filescatalog.PermissionFilesRead),
			string(filescatalog.PermissionFilesWrite),
			string(filescatalog.PermissionFilesDelete),
			string(filescatalog.PermissionFilesRestore),
		},
		Active:       true,
		TenantActive: true,
	}, nil
}

func (handlerSessions) Invalidate(context.Context, uuid.UUID) error { return nil }
func (handlerSessions) InvalidatePermissions(context.Context) error { return nil }

// handlerAudit satisfies the Guard's intent and completion contracts.
type handlerAudit struct{}

func (handlerAudit) Record(context.Context, audit.Event) error { return nil }
func (handlerAudit) RecordID(context.Context, audit.Event) (uuid.UUID, error) {
	return uuid.New(), nil
}
func (handlerAudit) Complete(context.Context, uuid.UUID, audit.Completion) error { return nil }

type filesHarness struct {
	app   *app.App
	token string
}

func newFilesHarness(t *testing.T, svc filesvc.UploadService, maxFileSize int64, allowed []string, flags features.Evaluator, bodyLimit int) *filesHarness {
	t.Helper()
	if bodyLimit <= 0 {
		bodyLimit = 1 << 20
	}
	cfg := &config.Config{
		App: config.AppConfig{
			Env:            "test",
			BodyLimit:      bodyLimit,
			JSONBodyLimit:  bodyLimit,
			RequestTimeout: 5 * time.Second,
		},
		JWT:       config.JWTConfig{Secret: handlerTestSecret},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 1000, StrictWindow: 60 * time.Second, NormalMax: 1000, NormalWindow: 60 * time.Second, RelaxedMax: 1000, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	module := filesmodule.New(fileshandler.NewUploadHandler(svc, maxFileSize, allowed))
	catalogs, err := app.BuildCatalogs(module)
	require.NoError(t, err)
	application := app.New(cfg, app.Deps{
		Cache:    cache.NewMemoryCache(),
		Sessions: handlerSessions{},
		Audit:    handlerAudit{},
		Flags:    flags,
		Catalogs: catalogs,
	})
	application.Register(module)

	tok, err := token.GenerateWithTenant(handlerTestID(1), handlerTestID(1), handlerTestSecret, 24*time.Hour)
	require.NoError(t, err)
	return &filesHarness{app: application, token: tok}
}

func (h *filesHarness) do(t *testing.T, req *http.Request) (resp *http.Response, body []byte) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.app.Fiber().Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func get(t *testing.T, h *filesHarness, path string) (resp *http.Response, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	require.NoError(t, err)
	return h.do(t, req)
}

func multipartBody(t *testing.T, filename string, content []byte) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return body, writer.FormDataContentType()
}

func upload(t *testing.T, h *filesHarness, filename string, content []byte) (resp *http.Response, body []byte) {
	t.Helper()
	payload, contentType := multipartBody(t, filename, content)
	req, err := http.NewRequest(http.MethodPost, "/api/v1/files", payload)
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	return h.do(t, req)
}

func decodeFile(t *testing.T, body []byte) dto.FileResponse {
	t.Helper()
	var file dto.FileResponse
	require.NoError(t, json.Unmarshal(body, &file))
	return file
}

func TestGetFile(t *testing.T) {
	svc := newMockUploadService()
	h := newFilesHarness(t, svc, 10*1024*1024, []string{"application/pdf", "image/jpeg"}, nil, 0)

	resp, body := get(t, h, "/api/v1/files/"+handlerTestID(1).String())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	file := decodeFile(t, body)
	assert.Equal(t, handlerTestID(1), file.ID)
	assert.Equal(t, "/api/v1/files/"+handlerTestID(1).String()+"/content", file.URL)
}

func TestGetFileNotFound(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	resp, body := get(t, h, "/api/v1/files/"+handlerTestID(999).String())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	var problem apperror.ProblemDetails
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, string(apperror.CodeResourceNotFound), problem.Code)
}

func TestGetFileInvalidID(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	resp, _ := get(t, h, "/api/v1/files/1")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGetFileContent(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	resp, body := get(t, h, "/api/v1/files/"+handlerTestID(1).String()+"/content")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/pdf", resp.Header.Get("Content-Type"))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), `filename="test.pdf"`)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Len(t, body, 1024)
}

func TestGetFileContentNotFound(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	resp, _ := get(t, h, "/api/v1/files/"+handlerTestID(999).String()+"/content")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestListFiles(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	resp, body := get(t, h, "/api/v1/files")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var page struct {
		Object string             `json:"object"`
		URL    string             `json:"url"`
		Data   []dto.FileResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	assert.Equal(t, "list", page.Object)
	assert.Equal(t, "/api/v1/files", page.URL)
	assert.Len(t, page.Data, 1)
}

func TestListFilesClampsLargeLimit(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	resp, _ := get(t, h, "/api/v1/files?limit=101")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the service clamps a limit above 100")
}

func TestDeleteFile(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	req, err := http.NewRequest(http.MethodDelete, "/api/v1/files/"+handlerTestID(1).String(), http.NoBody)
	require.NoError(t, err)
	resp, _ := h.do(t, req)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestDeleteFileNotFound(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	req, err := http.NewRequest(http.MethodDelete, "/api/v1/files/"+handlerTestID(999).String(), http.NoBody)
	require.NoError(t, err)
	resp, _ := h.do(t, req)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRestoreFile(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024, nil, nil, 0)

	req, err := http.NewRequest(http.MethodPost, "/api/v1/files/"+handlerTestID(1).String()+"/restore", http.NoBody)
	require.NoError(t, err)
	resp, body := h.do(t, req)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, handlerTestID(1), decodeFile(t, body).ID)
}

func TestCreateFile(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024,
		[]string{"application/pdf", "image/jpeg", "text/plain; charset=utf-8"}, nil, 0)

	resp, body := upload(t, h, "test.bin", []byte("test file content here"))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "/api/v1/files/"+handlerTestID(2).String(), resp.Header.Get("Location"))
	file := decodeFile(t, body)
	assert.Equal(t, handlerTestID(2), file.ID)
	assert.Equal(t, "text/plain; charset=utf-8", file.MimeType)
}

func TestCreateFileRequiresFile(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024,
		[]string{"application/pdf"}, nil, 0)

	resp, _ := upload(t, h, "", nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreateFileRejectsDisallowedMIME(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024,
		[]string{"image/jpeg"}, nil, 0)

	resp, _ := upload(t, h, "test.txt", []byte("plain text content"))
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
}

func TestCreateFileRejectsBodyOverLimit(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10*1024*1024,
		[]string{"text/plain; charset=utf-8"}, nil, 128)

	resp, _ := upload(t, h, "large.txt", bytes.Repeat([]byte("x"), 256))
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestCreateFileRejectsOversizedFile(t *testing.T) {
	h := newFilesHarness(t, newMockUploadService(), 10,
		[]string{"text/plain; charset=utf-8"}, nil, 0)

	resp, _ := upload(t, h, "large.txt", bytes.Repeat([]byte("x"), 32))
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}
