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

	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// mockAdminService is a manual mock for testing admin handlers.
type mockAdminService struct {
	users []admindto.UserResponse
	files []admindto.FileResponse
	stats *admindto.AdminStatsResponse
}

func newMockAdminService() *mockAdminService {
	return &mockAdminService{
		users: []admindto.UserResponse{
			{ID: 1, Email: "user1@example.com", Name: "User 1", Role: "user"},
			{ID: 2, Email: "user2@example.com", Name: "User 2", Role: "admin"},
		},
		files: []admindto.FileResponse{
			{ID: 1, OriginalName: "file.txt", MimeType: "text/plain", Size: 1024, URL: "http://localhost/files/1"},
		},
		stats: &admindto.AdminStatsResponse{
			ActiveUsers: 10, DeletedUsers: 2, TotalFiles: 5, TotalFileSize: 50000,
		},
	}
}

func (m *mockAdminService) ListUsers(_ context.Context, _ int, _ string) ([]admindto.UserResponse, bool, error) {
	return m.users, false, nil
}

func (m *mockAdminService) UpdateRole(_ context.Context, id int64, role string) (*admindto.UserResponse, error) {
	for i, u := range m.users {
		if u.ID == id {
			m.users[i].Role = role
			return &m.users[i], nil
		}
	}
	return nil, apperror.NewNotFound("user not found")
}

func (m *mockAdminService) BanUser(_ context.Context, id int64) error {
	for _, u := range m.users {
		if u.ID == id {
			return nil
		}
	}
	return apperror.NewNotFound("user not found")
}

func (m *mockAdminService) UnbanUser(_ context.Context, id int64) (*admindto.UserResponse, error) {
	for _, u := range m.users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, apperror.NewNotFound("user not found")
}

func (m *mockAdminService) ListFiles(_ context.Context, _ int, _ string) ([]admindto.FileResponse, bool, error) {
	return m.files, false, nil
}

func (m *mockAdminService) Download(_ context.Context, id int64) (*admindto.FileResponse, io.ReadCloser, error) {
	for i := range m.files {
		if m.files[i].ID == id {
			return &m.files[i], io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), int(m.files[i].Size)))), nil
		}
	}
	return nil, nil, apperror.NewNotFound("file not found")
}

func (m *mockAdminService) DeleteFile(_ context.Context, id int64) error {
	for i := range m.files {
		if m.files[i].ID == id {
			return nil
		}
	}
	return apperror.NewNotFound("file not found")
}

func (m *mockAdminService) RestoreFile(_ context.Context, id int64) (*admindto.FileResponse, error) {
	for i := range m.files {
		if m.files[i].ID == id {
			return &m.files[i], nil
		}
	}
	return nil, apperror.NewNotFound("file not found or not deleted")
}

func (m *mockAdminService) GetStats(_ context.Context) (*admindto.AdminStatsResponse, error) {
	return m.stats, nil
}

func setupAdminApp() *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	adminSvc := newMockAdminService()
	adminHandler := NewHandler(adminSvc)

	admin := app.Group("/admin", middleware.JWTAuth("test-secret"), middleware.RequireRole("admin"))
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/users", adminHandler.ListUsers)
	admin.Put("/users/:id/role", adminHandler.UpdateRole)
	admin.Post("/users/:id/ban", adminHandler.BanUser)
	admin.Post("/users/:id/unban", adminHandler.UnbanUser)
	admin.Get("/files", adminHandler.ListFiles)
	admin.Get("/files/:id/download", adminHandler.DownloadFile)
	admin.Delete("/files/:id", adminHandler.DeleteFile)
	admin.Post("/files/:id/restore", adminHandler.RestoreFile)

	return app
}

func adminToken() string {
	// Use an ID distinct from the users under test so self-protection guards
	// do not turn legitimate admin actions into 400s.
	t, _ := token.Generate(99, "admin@example.com", "admin", "test-secret", 24)
	return t
}

func TestAdminGetStats(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAdminGetStats_Forbidden(t *testing.T) {
	app := setupAdminApp()

	userToken, _ := token.Generate(1, "user@example.com", "user", "test-secret", 24)
	req, _ := http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func TestAdminListUsers(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("GET", "/admin/users", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAdminUpdateRole(t *testing.T) {
	app := setupAdminApp()

	body, _ := json.Marshal(admindto.UpdateRoleRequest{Role: "admin"})
	req, _ := http.NewRequest("PUT", "/admin/users/1/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAdminUpdateRole_NotFound(t *testing.T) {
	app := setupAdminApp()

	body, _ := json.Marshal(admindto.UpdateRoleRequest{Role: "admin"})
	req, _ := http.NewRequest("PUT", "/admin/users/999/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestAdminBanUser(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("POST", "/admin/users/1/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestAdminBanUser_NotFound(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("POST", "/admin/users/999/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestAdminUnbanUser(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("POST", "/admin/users/1/unban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAdminListFiles(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("GET", "/admin/files", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAdminUpdateRole_MalformedJSON(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("PUT", "/admin/users/1/role", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestAdminBanUser_InvalidID(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("POST", "/admin/users/abc/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestAdminUnbanUser_NotFound(t *testing.T) {
	app := setupAdminApp()

	req, _ := http.NewRequest("POST", "/admin/users/999/unban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestAdmin_SelfRoleAndBanGuards(t *testing.T) {
	app := setupAdminApp()
	tok := adminToken()

	body, _ := json.Marshal(admindto.UpdateRoleRequest{Role: "user"})
	req, _ := http.NewRequest("PUT", "/admin/users/99/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	req, _ = http.NewRequest("POST", "/admin/users/99/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestAdminFileRoutes(t *testing.T) {
	app := setupAdminApp()
	tok := adminToken()

	req, _ := http.NewRequest("GET", "/admin/files/1/download", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	req, _ = http.NewRequest("POST", "/admin/files/1/restore", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}
