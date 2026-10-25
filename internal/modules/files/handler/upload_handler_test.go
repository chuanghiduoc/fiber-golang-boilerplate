package handler

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// mockUploadService is a manual mock for testing the upload handler.
type mockUploadService struct {
	files map[int64]*filesvc.File
}

func newMockUploadService() *mockUploadService {
	return &mockUploadService{
		files: map[int64]*filesvc.File{
			1: {ID: 1, OriginalName: "test.pdf", MimeType: "application/pdf", Size: 1024},
		},
	}
}

func (m *mockUploadService) Upload(_ context.Context, _ int64, filename string, _ io.Reader, size int64, contentType string) (*filesvc.File, error) {
	return &filesvc.File{ID: 2, OriginalName: filename, MimeType: contentType, Size: size}, nil
}

func (m *mockUploadService) GetFileInfo(_ context.Context, id, _ int64) (*filesvc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.NewNotFound("file not found")
	}
	return f, nil
}

func (m *mockUploadService) Download(_ context.Context, id, _ int64) (*filesvc.File, io.ReadCloser, error) {
	if _, ok := m.files[id]; !ok {
		return nil, nil, apperror.NewNotFound("file not found")
	}
	return &filesvc.File{ID: id, OriginalName: "test.pdf", MimeType: "application/pdf", Size: 1024},
		io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), 1024))), nil
}

func (m *mockUploadService) List(_ context.Context, _ int64, _ int, _ string, _ bool) ([]filesvc.File, bool, error) {
	files := make([]filesvc.File, 0, len(m.files))
	for _, f := range m.files {
		files = append(files, *f)
	}
	return files, false, nil
}

func (m *mockUploadService) Delete(_ context.Context, id, _ int64) error {
	if _, ok := m.files[id]; !ok {
		return apperror.NewNotFound("file not found")
	}
	delete(m.files, id)
	return nil
}

func (m *mockUploadService) Restore(_ context.Context, id, _ int64) (*filesvc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.NewNotFound("file not found")
	}
	return f, nil
}

func userToken() string {
	t, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)
	return t
}

func setupUploadApp() *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})

	uploadSvc := newMockUploadService()
	uploadHandler := NewUploadHandler(uploadSvc, 10*1024*1024, []string{"application/pdf", "image/jpeg"})

	files := app.Group("/files", middleware.JWTAuth("test-secret"))
	files.Get("/", uploadHandler.List)
	files.Get("/:id", uploadHandler.GetInfo)
	files.Get("/:id/download", uploadHandler.Download)
	files.Delete("/:id", uploadHandler.Delete)
	files.Post("/:id/restore", uploadHandler.Restore)

	return app
}

func TestUploadHandler_GetInfo(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("GET", "/files/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestUploadHandler_GetInfo_NotFound(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("GET", "/files/999", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUploadHandler_Download(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("GET", "/files/1/download", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestUploadHandler_Download_NotFound(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("GET", "/files/999/download", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUploadHandler_List(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("GET", "/files/", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestUploadHandler_Delete(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("DELETE", "/files/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestUploadHandler_Delete_NotFound(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("DELETE", "/files/999", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUploadHandler_Upload(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})

	uploadSvc := newMockUploadService()
	uploadHandler := NewUploadHandler(uploadSvc, 10*1024*1024, []string{"application/pdf", "image/jpeg", "text/plain; charset=utf-8"})

	files := app.Group("/files", middleware.JWTAuth("test-secret"))
	files.Post("/upload", uploadHandler.Upload)

	// Build multipart body
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.bin")
	_, _ = part.Write([]byte("test file content here"))
	_ = writer.Close()

	req, _ := http.NewRequest("POST", "/files/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
}

func TestUploadHandler_Upload_NoFile(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})

	uploadSvc := newMockUploadService()
	uploadHandler := NewUploadHandler(uploadSvc, 10*1024*1024, []string{"application/pdf"})

	files := app.Group("/files", middleware.JWTAuth("test-secret"))
	files.Post("/upload", uploadHandler.Upload)

	req, _ := http.NewRequest("POST", "/files/upload", bytes.NewReader([]byte{}))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=----test")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestUploadHandler_Upload_DisallowedMIME(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})

	// Only allow image/jpeg
	uploadSvc := newMockUploadService()
	uploadHandler := NewUploadHandler(uploadSvc, 10*1024*1024, []string{"image/jpeg"})

	files := app.Group("/files", middleware.JWTAuth("test-secret"))
	files.Post("/upload", uploadHandler.Upload)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("plain text content"))
	_ = writer.Close()

	req, _ := http.NewRequest("POST", "/files/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnsupportedMediaType, resp.StatusCode)
}

func TestUploadHandler_RejectsOversizedFile(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	uploadHandler := NewUploadHandler(newMockUploadService(), 10, []string{"text/plain; charset=utf-8"})
	app.Post("/upload", middleware.JWTAuth("test-secret"), uploadHandler.Upload)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "large.txt")
	_, _ = part.Write(bytes.Repeat([]byte("x"), 32))
	_ = writer.Close()

	req, _ := http.NewRequest("POST", "/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusRequestEntityTooLarge, resp.StatusCode)
}
