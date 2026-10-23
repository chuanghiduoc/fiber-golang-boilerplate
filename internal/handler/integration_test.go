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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func setupIntegrationApp(t *testing.T) (*fiber.App, repository.UserRepository, func()) {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)

	refreshRepo := repository.NewRefreshTokenRepository(pool)
	appCache := cache.NewMemoryCache()
	txManager := database.NewTxManager(pool)

	userRepo := repository.NewUserRepository(pool)
	sessionStore := session.NewStore(userRepo, appCache)
	userSvc := service.NewUserService(userRepo, refreshRepo, false, appCache, txManager, sessionStore)
	userHandler := NewUserHandler(userSvc)

	fileRepo := repository.NewFileRepository(pool)
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	adminSvc := service.NewAdminService(userRepo, fileRepo, refreshRepo, store, txManager, sessionStore)
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

	users := app.Group("/users", middleware.JWTAuth("integration-secret", sessionStore))
	users.Get("/me", userHandler.GetMe)
	users.Get("/:id", userHandler.GetByID)
	users.Put("/:id", userHandler.Update)
	users.Delete("/:id", userHandler.Delete)

	admin := app.Group("/admin",
		middleware.JWTAuth("integration-secret", sessionStore),
		middleware.RequireRole("admin"),
	)
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/users", adminHandler.ListUsers)
	admin.Put("/users/:id/role", adminHandler.UpdateRole)
	admin.Post("/users/:id/ban", adminHandler.BanUser)
	admin.Post("/users/:id/unban", adminHandler.UnbanUser)

	return app, userRepo, cleanup
}

func TestIntegration_FullUserFlow(t *testing.T) {
	app, _, cleanup := setupIntegrationApp(t)
	defer cleanup()

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
	accessToken, _ := token.Generate(userID, "integration@test.com", "user", "integration-secret", 24)

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

func TestIntegration_AdminOperations(t *testing.T) {
	app, userRepo, cleanup := setupIntegrationApp(t)
	defer cleanup()

	// Register a regular user
	body, _ := json.Marshal(dto.RegisterRequest{
		Email:    "regular@test.com",
		Password: "Password1!",
		Name:     "Regular User",
	})
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var regular dto.UserResponse
	require.NoError(t, json.Unmarshal(respBody, &regular))
	regularToken, err := token.GenerateWithVersion(regular.ID, regular.Email, regular.Role, regular.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)

	// Create a real admin so the session validator has a current role/version
	// to compare against.
	hash, err := bcrypt.GenerateFromPassword([]byte("Admin123!"), bcrypt.MinCost)
	require.NoError(t, err)
	admin, err := userRepo.Create(context.Background(), sqlc.CreateUserParams{
		Email:        "admin@test.com",
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         "Integration Admin",
	})
	require.NoError(t, err)
	admin, err = userRepo.UpdateRole(context.Background(), sqlc.UpdateUserRoleParams{
		ID:   admin.ID,
		Role: dto.RoleAdmin,
	})
	require.NoError(t, err)
	adminToken, err := token.GenerateWithVersion(admin.ID, admin.Email, admin.Role, admin.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)

	// Get stats
	req, _ = http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// List users
	req, _ = http.NewRequest("GET", "/admin/users", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Ban user (soft delete) while they are still a regular user.
	req, _ = http.NewRequest("POST", "/admin/users/1/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	// A pre-ban access token must stop working immediately.
	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+regularToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// Unban user (restore)
	req, _ = http.NewRequest("POST", "/admin/users/1/unban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// The pre-ban token stays invalid after restore (token version bumped).
	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+regularToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// A token issued for the restored version works.
	restored, err := userRepo.GetByID(context.Background(), regular.ID)
	require.NoError(t, err)
	restoredToken, err := token.GenerateWithVersion(restored.ID, restored.Email, restored.Role, restored.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)
	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+restoredToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Update role after unbanning, otherwise the last-admin guard would (by
	// design) reject banning the only active admin.
	body, _ = json.Marshal(dto.UpdateRoleRequest{Role: "admin"})
	req, _ = http.NewRequest("PUT", "/admin/users/1/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// A separate regular user gets 403 (not 401): their session is valid, but
	// their role is insufficient.
	body, _ = json.Marshal(dto.RegisterRequest{
		Email:    "other@test.com",
		Password: "Password1!",
		Name:     "Other User",
	})
	req, _ = http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode)

	var other dto.UserResponse
	respBody, _ = io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(respBody, &other))
	userToken, err := token.GenerateWithVersion(other.ID, other.Email, other.Role, other.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)

	req, _ = http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}
