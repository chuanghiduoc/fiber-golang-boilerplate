//go:build integration

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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func setupIntegrationApp(t *testing.T) (*fiber.App, func()) {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)

	userRepo := repository.NewUserRepository(pool)
	userSvc := service.NewUserService(userRepo, false)
	userHandler := NewUserHandler(userSvc)

	fileRepo := repository.NewFileRepository(pool)
	adminSvc := service.NewAdminService(userRepo, fileRepo, nil)
	adminHandler := NewAdminHandler(adminSvc)

	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	app.Post("/auth/register", func(c fiber.Ctx) error {
		var req dto.RegisterRequest
		if err := bindAndValidate(c, &req); err != nil {
			return err
		}
		user, err := userSvc.Register(c.Context(), req)
		if err != nil {
			return err
		}
		return response.Created(c, user)
	})

	users := app.Group("/users", middleware.JWTAuth("integration-secret"))
	users.Get("/me", userHandler.GetMe)
	users.Get("/:id", userHandler.GetByID)
	users.Put("/:id", userHandler.Update)
	users.Delete("/:id", userHandler.Delete)

	admin := app.Group("/admin",
		middleware.JWTAuth("integration-secret"),
		middleware.RequireRole("admin"),
	)
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/users", adminHandler.ListUsers)
	admin.Put("/users/:id/role", adminHandler.UpdateRole)
	admin.Post("/users/:id/ban", adminHandler.BanUser)
	admin.Post("/users/:id/unban", adminHandler.UnbanUser)

	return app, cleanup
}

func TestIntegration_FullUserFlow(t *testing.T) {
	app, cleanup := setupIntegrationApp(t)
	defer cleanup()

	// 1. Register
	body, _ := json.Marshal(dto.RegisterRequest{
		Email:    "integration@test.com",
		Password: "Password1!",
		Name:     "Integration User",
	})
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)

	var registerResp response.Response
	respBody, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(respBody, &registerResp))
	assert.True(t, registerResp.Success)

	// Extract user ID from response
	userData, _ := json.Marshal(registerResp.Data)
	var userResp dto.UserResponse
	require.NoError(t, json.Unmarshal(userData, &userResp))
	userID := userResp.ID

	// 2. Get user (with JWT)
	accessToken, _ := token.Generate(userID, "integration@test.com", "user", "integration-secret", 24)

	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// 3. Update user
	name := "Updated Name"
	body, _ = json.Marshal(dto.UpdateUserRequest{Name: &name})
	req, _ = http.NewRequest("PUT", "/users/1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// 4. Soft delete user
	req, _ = http.NewRequest("DELETE", "/users/1", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	// 5. User no longer found
	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestIntegration_AdminOperations(t *testing.T) {
	app, cleanup := setupIntegrationApp(t)
	defer cleanup()

	// Register a regular user
	body, _ := json.Marshal(dto.RegisterRequest{
		Email:    "regular@test.com",
		Password: "Password1!",
		Name:     "Regular User",
	})
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)

	// Admin token (we'll use user ID 999 as admin — doesn't need to exist for token generation)
	adminToken, _ := token.Generate(999, "admin@test.com", "admin", "integration-secret", 24)

	// Get stats
	req, _ = http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// List users
	req, _ = http.NewRequest("GET", "/admin/users", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Update role
	body, _ = json.Marshal(dto.UpdateRoleRequest{Role: "admin"})
	req, _ = http.NewRequest("PUT", "/admin/users/1/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Ban user (soft delete)
	req, _ = http.NewRequest("POST", "/admin/users/1/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	// Unban user (restore)
	req, _ = http.NewRequest("POST", "/admin/users/1/unban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Non-admin gets 403
	userToken, _ := token.Generate(1, "regular@test.com", "user", "integration-secret", 24)
	req, _ = http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}
