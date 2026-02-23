package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// mockUserService is a manual mock for testing handlers.
type mockUserService struct {
	users map[int64]*dto.UserResponse
}

func newMockService() *mockUserService {
	return &mockUserService{
		users: map[int64]*dto.UserResponse{
			1: {ID: 1, Email: "test@example.com", Name: "Test User", Role: "user"},
		},
	}
}

func (m *mockUserService) Register(_ context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	return &dto.UserResponse{ID: 2, Email: req.Email, Name: req.Name, Role: "user"}, nil
}

func (m *mockUserService) Authenticate(_ context.Context, req dto.LoginRequest) (*sqlc.User, error) {
	if req.Email == "test@example.com" && req.Password == "Password1!" {
		return &sqlc.User{ID: 1, Email: "test@example.com", Name: "Test User", Role: "user"}, nil
	}
	return nil, apperror.NewUnauthorized("invalid email or password")
}

func (m *mockUserService) GetByID(_ context.Context, id int64) (*dto.UserResponse, error) {
	user, ok := m.users[id]
	if !ok {
		return nil, apperror.NewNotFound("user not found")
	}
	return user, nil
}

func (m *mockUserService) List(_ context.Context, _, _ int) ([]dto.UserResponse, int64, error) {
	users := make([]dto.UserResponse, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, *u)
	}
	return users, int64(len(users)), nil
}

func (m *mockUserService) Update(_ context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error) {
	user, ok := m.users[id]
	if !ok {
		return nil, apperror.NewNotFound("user not found")
	}
	if req.Name != nil {
		user.Name = *req.Name
	}
	return user, nil
}

func (m *mockUserService) Delete(_ context.Context, id int64) error {
	if _, ok := m.users[id]; !ok {
		return apperror.NewNotFound("user not found")
	}
	delete(m.users, id)
	return nil
}

func (m *mockUserService) FindOrCreateByGoogle(_ context.Context, _, email, name string) (*sqlc.User, error) {
	return &sqlc.User{ID: 1, Email: email, Name: name, Role: "user"}, nil
}

func (m *mockUserService) ChangePassword(_ context.Context, _ int64, _ dto.ChangePasswordRequest) error {
	return nil
}

// mockRefreshTokenService is a manual mock for testing handlers.
type mockRefreshTokenService struct{}

func (m *mockRefreshTokenService) Create(_ context.Context, _ int64) (string, error) {
	return "mock-refresh-token", nil
}

func (m *mockRefreshTokenService) Verify(_ context.Context, tokenStr string) (*sqlc.RefreshToken, error) {
	if tokenStr == "valid-refresh-token" {
		return &sqlc.RefreshToken{ID: 1, UserID: 1, Token: tokenStr}, nil
	}
	return nil, apperror.NewUnauthorized("invalid refresh token")
}

func (m *mockRefreshTokenService) Revoke(_ context.Context, _ string) error {
	return nil
}

func (m *mockRefreshTokenService) RevokeAllByUserID(_ context.Context, _ int64) error {
	return nil
}

// mockPasswordResetService is a manual mock for testing handlers.
type mockPasswordResetService struct{}

func (m *mockPasswordResetService) ForgotPassword(_ context.Context, _ dto.ForgotPasswordRequest) error {
	return nil
}

func (m *mockPasswordResetService) ResetPassword(_ context.Context, _ dto.ResetPasswordRequest) error {
	return nil
}

// mockEmailVerificationService is a manual mock for testing handlers.
type mockEmailVerificationService struct{}

func (m *mockEmailVerificationService) SendVerification(_ context.Context, _ int64, _ string) error {
	return nil
}

func (m *mockEmailVerificationService) Verify(_ context.Context, _ string) error {
	return nil
}

func (m *mockEmailVerificationService) ResendVerification(_ context.Context, _ string) error {
	return nil
}

func setupApp(svc *mockUserService) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	refreshSvc := &mockRefreshTokenService{}
	resetSvc := &mockPasswordResetService{}
	emailVerifSvc := &mockEmailVerificationService{}
	authHandler := NewAuthHandler(svc, refreshSvc, resetSvc, emailVerifSvc, "test-secret", 24, nil)
	userHandler := NewUserHandler(svc)

	app.Post("/auth/register", authHandler.Register)
	app.Post("/auth/login", authHandler.Login)
	app.Post("/auth/refresh", authHandler.Refresh)
	app.Post("/auth/logout", authHandler.Logout)
	app.Post("/auth/forgot-password", authHandler.ForgotPassword)
	app.Post("/auth/reset-password", authHandler.ResetPassword)
	app.Post("/auth/verify-email", authHandler.VerifyEmail)
	app.Post("/auth/resend-verification", authHandler.ResendVerification)
	app.Get("/auth/google", authHandler.GoogleRedirect)
	app.Get("/auth/google/callback", authHandler.GoogleCallback)

	users := app.Group("/users", middleware.JWTAuth("test-secret"))
	users.Get("/me", userHandler.GetMe)
	users.Put("/me", userHandler.UpdateMe)
	users.Put("/me/password", userHandler.ChangePassword)
	users.Get("/", userHandler.List)
	users.Get("/:id", userHandler.GetByID)
	users.Put("/:id", userHandler.Update)
	users.Delete("/:id", userHandler.Delete)

	return app
}

func TestRegisterHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.RegisterRequest{
		Email:    "new@example.com",
		Password: "Password1!",
		Name:     "New User",
	})

	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
}

func TestLoginHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.LoginRequest{
		Email:    "test@example.com",
		Password: "Password1!",
	})

	req, _ := http.NewRequest("POST", "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestLoginHandler_InvalidCredentials(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.LoginRequest{
		Email:    "test@example.com",
		Password: "WrongPassword2@",
	})

	req, _ := http.NewRequest("POST", "/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func TestGetMe_Unauthorized(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("GET", "/users/me", http.NoBody)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func TestGetMe_Authorized(t *testing.T) {
	app := setupApp(newMockService())

	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)

	req, _ := http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestGetByID_NotFound(t *testing.T) {
	app := setupApp(newMockService())

	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)

	req, _ := http.NewRequest("GET", "/users/999", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUpdate_Forbidden(t *testing.T) {
	app := setupApp(newMockService())

	// User 1 trying to update user 2
	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)

	body, _ := json.Marshal(dto.UpdateUserRequest{})
	req, _ := http.NewRequest("PUT", "/users/2", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func TestUpdate_AdminBypass(t *testing.T) {
	app := setupApp(newMockService())

	// Admin trying to update user 1
	accessToken, _ := token.Generate(2, "admin@example.com", "admin", "test-secret", 24)

	name := "Updated Name"
	body, _ := json.Marshal(dto.UpdateUserRequest{Name: &name})
	req, _ := http.NewRequest("PUT", "/users/1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestDelete_Forbidden(t *testing.T) {
	app := setupApp(newMockService())

	// User 1 trying to delete user 2
	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)

	req, _ := http.NewRequest("DELETE", "/users/2", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func TestDelete_AdminBypass(t *testing.T) {
	app := setupApp(newMockService())

	// Admin trying to delete user 1
	accessToken, _ := token.Generate(2, "admin@example.com", "admin", "test-secret", 24)

	req, _ := http.NewRequest("DELETE", "/users/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestRegisterHandler_ValidationError(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(map[string]string{
		"email":    "invalid",
		"password": "short",
	})

	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)
}

func TestRefreshHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.RefreshRequest{
		RefreshToken: "valid-refresh-token",
	})

	req, _ := http.NewRequest("POST", "/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestRefreshHandler_InvalidToken(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.RefreshRequest{
		RefreshToken: "invalid-token",
	})

	req, _ := http.NewRequest("POST", "/auth/refresh", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func TestLogoutHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.RefreshRequest{
		RefreshToken: "some-token",
	})

	req, _ := http.NewRequest("POST", "/auth/logout", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestForgotPasswordHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.ForgotPasswordRequest{
		Email: "test@example.com",
	})

	req, _ := http.NewRequest("POST", "/auth/forgot-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestResetPasswordHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.ResetPasswordRequest{
		Token:    "valid-reset-token",
		Password: "NewPassword1!",
	})

	req, _ := http.NewRequest("POST", "/auth/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestVerifyEmailHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.VerifyEmailRequest{
		Token: "valid-verification-token",
	})

	req, _ := http.NewRequest("POST", "/auth/verify-email", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestResendVerificationHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.ResendVerificationRequest{
		Email: "test@example.com",
	})

	req, _ := http.NewRequest("POST", "/auth/resend-verification", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// mockAdminService
// ---------------------------------------------------------------------------

type mockAdminService struct {
	users []dto.UserResponse
	files []dto.FileResponse
	stats *dto.AdminStatsResponse
}

func newMockAdminService() *mockAdminService {
	return &mockAdminService{
		users: []dto.UserResponse{
			{ID: 1, Email: "user1@example.com", Name: "User 1", Role: "user"},
			{ID: 2, Email: "user2@example.com", Name: "User 2", Role: "admin"},
		},
		files: []dto.FileResponse{
			{ID: 1, OriginalName: "file.txt", MimeType: "text/plain", Size: 1024, URL: "http://localhost/files/1"},
		},
		stats: &dto.AdminStatsResponse{
			ActiveUsers: 10, DeletedUsers: 2, TotalFiles: 5, TotalFileSize: 50000,
		},
	}
}

func (m *mockAdminService) ListUsers(_ context.Context, _, _ int) ([]dto.UserResponse, int64, error) {
	return m.users, int64(len(m.users)), nil
}

func (m *mockAdminService) UpdateRole(_ context.Context, id int64, role string) (*dto.UserResponse, error) {
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

func (m *mockAdminService) UnbanUser(_ context.Context, id int64) (*dto.UserResponse, error) {
	for _, u := range m.users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, apperror.NewNotFound("user not found")
}

func (m *mockAdminService) ListFiles(_ context.Context, _, _ int) ([]dto.FileResponse, int64, error) {
	return m.files, int64(len(m.files)), nil
}

func (m *mockAdminService) GetStats(_ context.Context) (*dto.AdminStatsResponse, error) {
	return m.stats, nil
}

func setupAdminApp() *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	adminSvc := newMockAdminService()
	adminHandler := NewAdminHandler(adminSvc)

	admin := app.Group("/admin", middleware.JWTAuth("test-secret"), middleware.RequireRole("admin"))
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/users", adminHandler.ListUsers)
	admin.Put("/users/:id/role", adminHandler.UpdateRole)
	admin.Post("/users/:id/ban", adminHandler.BanUser)
	admin.Post("/users/:id/unban", adminHandler.UnbanUser)
	admin.Get("/files", adminHandler.ListFiles)

	return app
}

func adminToken() string {
	t, _ := token.Generate(1, "admin@example.com", "admin", "test-secret", 24)
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

	body, _ := json.Marshal(dto.UpdateRoleRequest{Role: "admin"})
	req, _ := http.NewRequest("PUT", "/admin/users/1/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestAdminUpdateRole_NotFound(t *testing.T) {
	app := setupAdminApp()

	body, _ := json.Marshal(dto.UpdateRoleRequest{Role: "admin"})
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

// ---------------------------------------------------------------------------
// Additional User Handler tests
// ---------------------------------------------------------------------------

func userToken() string {
	t, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)
	return t
}

func TestListUsersHandler(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("GET", "/users/", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestUpdateMeHandler(t *testing.T) {
	app := setupApp(newMockService())

	name := "Updated Name"
	body, _ := json.Marshal(dto.UpdateUserRequest{Name: &name})
	req, _ := http.NewRequest("PUT", "/users/me", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestChangePasswordHandler(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.ChangePasswordRequest{
		CurrentPassword: "OldPassword1!",
		NewPassword:     "NewPassword1!",
	})
	req, _ := http.NewRequest("PUT", "/users/me/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestUpdate_OwnProfile(t *testing.T) {
	app := setupApp(newMockService())

	name := "Self Updated"
	body, _ := json.Marshal(dto.UpdateUserRequest{Name: &name})
	req, _ := http.NewRequest("PUT", "/users/1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestDelete_OwnAccount(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("DELETE", "/users/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// mockUploadService
// ---------------------------------------------------------------------------

type mockUploadService struct {
	files map[int64]*dto.FileResponse
}

func newMockUploadService() *mockUploadService {
	return &mockUploadService{
		files: map[int64]*dto.FileResponse{
			1: {ID: 1, OriginalName: "test.pdf", MimeType: "application/pdf", Size: 1024, URL: "http://localhost/files/1"},
		},
	}
}

func (m *mockUploadService) Upload(_ context.Context, _ int64, filename string, _ io.Reader, size int64, contentType string) (*dto.FileResponse, error) {
	return &dto.FileResponse{ID: 2, OriginalName: filename, MimeType: contentType, Size: size, URL: "http://localhost/files/2"}, nil
}

func (m *mockUploadService) GetFileInfo(_ context.Context, id, _ int64) (*dto.FileResponse, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.NewNotFound("file not found")
	}
	return f, nil
}

func (m *mockUploadService) Download(_ context.Context, id, _ int64) (*sqlc.File, io.ReadCloser, error) {
	if _, ok := m.files[id]; !ok {
		return nil, nil, apperror.NewNotFound("file not found")
	}
	return &sqlc.File{ID: id, OriginalName: "test.pdf", MimeType: "application/pdf", Size: 1024},
		io.NopCloser(bytes.NewReader([]byte("file content"))), nil
}

func (m *mockUploadService) List(_ context.Context, _ int64, _, _ int) ([]dto.FileResponse, int64, error) {
	files := make([]dto.FileResponse, 0, len(m.files))
	for _, f := range m.files {
		files = append(files, *f)
	}
	return files, int64(len(files)), nil
}

func (m *mockUploadService) Delete(_ context.Context, id, _ int64) error {
	if _, ok := m.files[id]; !ok {
		return apperror.NewNotFound("file not found")
	}
	delete(m.files, id)
	return nil
}

func setupUploadApp() *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	uploadSvc := newMockUploadService()
	uploadHandler := NewUploadHandler(uploadSvc, 10*1024*1024, []string{"application/pdf", "image/jpeg"})

	files := app.Group("/files", middleware.JWTAuth("test-secret"))
	files.Get("/", uploadHandler.List)
	files.Get("/:id", uploadHandler.GetInfo)
	files.Get("/:id/download", uploadHandler.Download)
	files.Delete("/:id", uploadHandler.Delete)

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

// ---------------------------------------------------------------------------
// Google OAuth handler tests (nil OAuth provider)
// ---------------------------------------------------------------------------

func TestGoogleRedirect_NotConfigured(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("GET", "/auth/google", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestGoogleCallback_NotConfigured(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("GET", "/auth/google/callback?code=abc&state=xyz", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// bindAndValidate error paths
// ---------------------------------------------------------------------------

func TestRegisterHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader([]byte("{invalid json")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestLoginHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/login", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestLogoutHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/logout", bytes.NewReader([]byte("not-json")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestRefreshHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/refresh", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestForgotPasswordHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/forgot-password", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestResetPasswordHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/reset-password", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestVerifyEmailHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/verify-email", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestResendVerificationHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/resend-verification", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
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

// ---------------------------------------------------------------------------
// paramID invalid tests
// ---------------------------------------------------------------------------

func TestGetByID_InvalidID(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("GET", "/users/abc", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestGetByID_Success(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("GET", "/users/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
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

// ---------------------------------------------------------------------------
// Upload handler with multipart form
// ---------------------------------------------------------------------------

func TestUploadHandler_Upload(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
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
		ErrorHandler: apperror.FiberErrorHandler,
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
		ErrorHandler: apperror.FiberErrorHandler,
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
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// UpdateMe with malformed JSON
// ---------------------------------------------------------------------------

func TestUpdateMeHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("PUT", "/users/me", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestChangePasswordHandler_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("PUT", "/users/me/password", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestUpdate_MalformedJSON(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("PUT", "/users/1", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestUploadHandler_Download_NotFound(t *testing.T) {
	app := setupUploadApp()

	req, _ := http.NewRequest("GET", "/files/999/download", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}
