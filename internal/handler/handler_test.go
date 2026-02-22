package handler

import (
	"bytes"
	"context"
	"encoding/json"
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

	users := app.Group("/users", middleware.JWTAuth("test-secret"))
	users.Get("/me", userHandler.GetMe)
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
