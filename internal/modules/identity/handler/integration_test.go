//go:build integration

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func setupIntegrationApp(t *testing.T) *fiber.App {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	refreshRepo := identityrepo.NewRefreshTokenRepository(pool)
	appCache := cache.NewMemoryCache()
	txManager := database.NewTxManager(pool)

	userRepo := identityrepo.NewUserRepository(pool)
	sessionStore := session.NewStore(userRepo, appCache)
	userSvc := identityservice.NewUserService(userRepo, refreshRepo, false, appCache, txManager, audit.Noop{}, sessionStore)
	userHandler := NewUserHandler(userSvc)

	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	app.Post("/auth/register", func(c fiber.Ctx) error {
		var req dto.RegisterRequest
		if err := httpx.BindAndValidate(c, &req); err != nil {
			return err
		}
		user, err := userSvc.Register(c.Context(), req)
		if err != nil {
			return err
		}
		return response.Created(c, user)
	})

	users := app.Group("/users", middleware.JWTAuth("integration-secret", sessionStore))
	users.Get("/me", userHandler.GetMe)
	users.Get("/:id", userHandler.GetByID)
	users.Put("/:id", userHandler.Update)
	users.Delete("/:id", userHandler.Delete)

	return app
}

func TestIntegration_FullUserFlow(t *testing.T) {
	app := setupIntegrationApp(t)

	// 1. Register
	body, _ := json.Marshal(dto.RegisterRequest{
		Email:    "integration@test.com",
		Password: "Password1!",
		Name:     "Integration User",
	})
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)

	// Register returns the created user resource directly (no envelope).
	respBody, _ := io.ReadAll(resp.Body)
	var userResp dto.UserResponse
	require.NoError(t, json.Unmarshal(respBody, &userResp))
	userID := userResp.ID

	// 2. Get user (with JWT)
	accessToken, _ := token.GenerateWithTenant(userID, 1, "integration@test.com", "user", "integration-secret", 24)

	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// 3. Update user
	name := "Updated Name"
	body, _ = json.Marshal(dto.UpdateUserRequest{Name: &name})
	req, _ = http.NewRequest("PUT", "/users/1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// 4. Soft delete user
	req, _ = http.NewRequest("DELETE", "/users/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	// 5. The deleted user's access token is rejected by the session validator
	//    immediately, rather than remaining usable until JWT expiry.
	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}
