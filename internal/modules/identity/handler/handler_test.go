package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// mockUserService is a manual mock for testing handlers.
type mockUserService struct {
	users                           map[int64]*dto.UserResponse
	hasMore                         bool
	registrationEmailsTransactional bool
	// googleAllowSignUp records the allowSignUp argument of each Google call.
	googleAllowSignUp []bool
}

func newMockService() *mockUserService {
	return &mockUserService{
		users: map[int64]*dto.UserResponse{
			1: {ID: 1, TenantID: 1, Email: "test@example.com", Name: "Test User", Role: "user", HasPassword: true},
		},
	}
}

func (m *mockUserService) Register(_ context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	return &dto.UserResponse{ID: 2, TenantID: 1, Email: req.Email, Name: req.Name, Role: "user"}, nil
}

func (m *mockUserService) Authenticate(_ context.Context, req dto.LoginRequest) (*dto.UserResponse, error) {
	if req.Email == "test@example.com" && req.Password == "Password1!" {
		return &dto.UserResponse{ID: 1, TenantID: 1, Email: "test@example.com", Name: "Test User", Role: "user"}, nil
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

func (m *mockUserService) List(_ context.Context, _ int, _ string) ([]dto.UserResponse, bool, error) {
	users := make([]dto.UserResponse, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, *u)
	}
	return users, m.hasMore, nil
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

func (m *mockUserService) DeleteAccount(_ context.Context, id int64) error {
	if _, ok := m.users[id]; !ok {
		return apperror.NewNotFound("user not found")
	}
	delete(m.users, id)
	return nil
}

func (m *mockUserService) RemoveMember(ctx context.Context, id int64) error {
	return m.DeleteAccount(ctx, id)
}

func (m *mockUserService) SignInWithGoogle(ctx context.Context, req service.GoogleSignIn) (*service.GoogleSignInResult, error) {
	m.googleAllowSignUp = append(m.googleAllowSignUp, req.AllowSignUp)
	user := &dto.UserResponse{ID: 1, TenantID: 1, Email: req.Email, Name: req.Name, Role: "user"}
	if req.Complete != nil {
		if err := req.Complete(ctx, user); err != nil {
			return nil, err
		}
	}
	return &service.GoogleSignInResult{
		User:   user,
		Tokens: service.TokenPair{AccessToken: "mock-access-token", RefreshToken: "mock-refresh-token", SessionID: 1},
	}, nil
}

func (m *mockUserService) ChangePassword(_ context.Context, _ int64, _ dto.ChangePasswordRequest) error {
	return nil
}

func (m *mockUserService) RevokeSessions(_ context.Context, _ int64) error {
	return nil
}

func (m *mockUserService) VerifyPassword(_ context.Context, _ int64, password string) error {
	if password == "Password1!" {
		return nil
	}
	return apperror.NewBadRequest("current password is incorrect")
}

func (m *mockUserService) RegistrationEmailsTransactional() bool {
	return m.registrationEmailsTransactional
}

// mockPasswordResetService is a manual mock for testing handlers.
type mockPasswordResetService struct{}

func (m *mockPasswordResetService) ForgotPassword(_ context.Context, _ dto.ForgotPasswordRequest) error {
	return nil
}

func (m *mockPasswordResetService) ResetPassword(_ context.Context, _ dto.ResetPasswordRequest) error {
	return nil
}

func (m *mockPasswordResetService) HandleResetRequestJob(_ context.Context, _ json.RawMessage) error {
	return nil
}

// mockEmailVerificationService is a manual mock for testing handlers.
type mockEmailVerificationService struct {
	sendVerificationCalls atomic.Int64
}

func (m *mockEmailVerificationService) SendVerification(_ context.Context, _ int64, _ string) error {
	m.sendVerificationCalls.Add(1)
	return nil
}

func (m *mockEmailVerificationService) SendVerificationCallCount() int {
	return int(m.sendVerificationCalls.Load())
}

func (m *mockEmailVerificationService) Verify(_ context.Context, _ string) error {
	return nil
}

func (m *mockEmailVerificationService) ResendVerification(_ context.Context, _ string) error {
	return nil
}

func (m *mockEmailVerificationService) Invalidate(_ context.Context, _ int64) error {
	return nil
}

func setupApp(svc *mockUserService) *fiber.App {
	return setupAppWithEmailService(svc, &mockEmailVerificationService{})
}

func setupAppWithEmailService(svc *mockUserService, emailVerifSvc *mockEmailVerificationService) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})

	refreshSvc := &mockSessions{}
	resetSvc := &mockPasswordResetService{}
	authHandler := newTestAuthHandler(svc, refreshSvc, resetSvc, emailVerifSvc, "test-secret", 24, false, nil)
	userHandler := NewUserHandler(svc)

	app.Post("/auth/register", authHandler.Register)
	app.Post("/auth/login", authHandler.Login)
	app.Post("/auth/refresh", authHandler.Refresh)
	app.Post("/auth/logout", authHandler.Logout)
	app.Post("/auth/logout-all", middleware.JWTAuth("test-secret"), authHandler.LogoutAll)
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

func TestRegisterHandler_VerificationEmailDispatch(t *testing.T) {
	register := func(t *testing.T, app *fiber.App) {
		t.Helper()
		body, _ := json.Marshal(dto.RegisterRequest{
			Email:    "new@example.com",
			Password: "Password1!",
			Name:     "New User",
		})
		req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, fiber.StatusCreated, resp.StatusCode)
	}

	t.Run("sends async verification when registration is not transactional", func(t *testing.T) {
		svc := newMockService()
		emailSvc := &mockEmailVerificationService{}
		app := setupAppWithEmailService(svc, emailSvc)

		register(t, app)

		require.Eventually(t, func() bool {
			return emailSvc.SendVerificationCallCount() == 1
		}, time.Second, 5*time.Millisecond, "fallback registration must send one verification email")
	})

	t.Run("skips async verification when registration is transactional", func(t *testing.T) {
		svc := newMockService()
		svc.registrationEmailsTransactional = true
		emailSvc := &mockEmailVerificationService{}
		app := setupAppWithEmailService(svc, emailSvc)

		register(t, app)

		require.Never(t, func() bool {
			return emailSvc.SendVerificationCallCount() > 0
		}, 100*time.Millisecond, 5*time.Millisecond, "transactional registration must not send a duplicate email")
	})
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

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)

	req, _ := http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestGetByID_NotFound(t *testing.T) {
	app := setupApp(newMockService())

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)

	req, _ := http.NewRequest("GET", "/users/999", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUpdate_Forbidden(t *testing.T) {
	app := setupApp(newMockService())

	// User 1 trying to update user 2
	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)

	body, _ := json.Marshal(dto.UpdateUserRequest{})
	req, _ := http.NewRequest("PUT", "/users/2", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func TestDelete_Forbidden(t *testing.T) {
	app := setupApp(newMockService())

	// User 1 trying to delete user 2
	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)

	req, _ := http.NewRequest("DELETE", "/users/2", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

func TestDelete_AdminBypass(t *testing.T) {
	app := setupApp(newMockService())

	// Admin trying to delete user 1
	accessToken, _ := token.GenerateWithTenant(2, 1, "admin@example.com", "admin", "test-secret", 24)

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

func userToken() string {
	t, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)
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

// TestCrossUserAccess_ScopedAPIKeyCannotUseOwnerRole guards against an API key
// inheriting its owner's admin role: an admin key scoped to profile access
// only must not read, update, or delete other users.
func TestCrossUserAccess_ScopedAPIKeyCannotUseOwnerRole(t *testing.T) {
	svc := newMockService()
	userHandler := NewUserHandler(svc)

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	users := app.Group("/users", func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 2)
		fiber.Locals[string](c, "role", authz.RoleAdmin)
		authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(2, 1, authz.RoleAdmin,
			identitycatalog.PermissionProfileRead, identitycatalog.PermissionProfileWrite))
		return c.Next()
	})
	users.Get("/:id", userHandler.GetByID)
	users.Put("/:id", userHandler.Update)
	users.Delete("/:id", userHandler.Delete)

	name := "Hijacked"
	body, _ := json.Marshal(dto.UpdateUserRequest{Name: &name})
	cases := []struct {
		method string
		body   []byte
		want   int
	}{
		{http.MethodGet, nil, fiber.StatusNotFound},
		{http.MethodPut, body, fiber.StatusForbidden},
		{http.MethodDelete, nil, fiber.StatusForbidden},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, "/users/1", bytes.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, tc.want, resp.StatusCode, tc.method)
	}
	assert.Equal(t, "Test User", svc.users[1].Name)
}
