//go:build integration

package integration_test

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

	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	adminservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
	filesadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func setupIntegrationApp(t *testing.T) (*fiber.App, identityrepo.UserRepository) {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	refreshRepo := identityrepo.NewRefreshTokenRepository(pool)
	appCache := cache.NewMemoryCache()
	txManager := database.NewTxManager(pool)
	auditRecorder := audit.NewPostgresRecorder(pool)

	userRepo := identityrepo.NewUserRepository(pool)
	sessionStore := session.NewStore(userRepo, appCache)
	userSvc := identityservice.NewUserService(userRepo, refreshRepo, false, appCache, txManager, auditRecorder, sessionStore)
	userHandler := identityhandler.NewUserHandler(userSvc)

	fileRepo := filesrepo.NewFileRepository(pool)
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	identityAdmin := identityadapter.NewAdminIdentity(
		identityservice.NewAdminUserService(userRepo, refreshRepo, txManager, auditRecorder, sessionStore),
	)
	filesAdmin := filesadapter.NewAdminFiles(filesvc.NewAdminFileService(fileRepo, filesrepo.NewTransactor(txManager, auditRecorder, nil), store))
	adminHandler := adminhandler.NewHandler(adminservice.NewService(identityAdmin, filesAdmin))

	app := fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})

	app.Post("/auth/register", func(c fiber.Ctx) error {
		var req identitydto.RegisterRequest
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

	admin := app.Group("/admin",
		middleware.JWTAuth("integration-secret", sessionStore),
		middleware.RequireRole("admin"),
	)
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/users", adminHandler.ListUsers)
	admin.Put("/users/:id/role", adminHandler.UpdateRole)
	admin.Post("/users/:id/ban", adminHandler.BanUser)
	admin.Post("/users/:id/unban", adminHandler.UnbanUser)

	return app, userRepo
}

func TestIntegration_AdminOperations(t *testing.T) {
	app, userRepo := setupIntegrationApp(t)

	// Register a regular user.
	body, _ := json.Marshal(identitydto.RegisterRequest{
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
	var regular identitydto.UserResponse
	require.NoError(t, json.Unmarshal(respBody, &regular))
	regularToken, err := token.GenerateWithVersionAndTenant(regular.ID, 1, regular.Email, regular.Role, regular.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)

	// Create a real admin so the session validator has a current role/version
	// to compare against.
	hash, err := bcrypt.GenerateFromPassword([]byte("Admin123!"), bcrypt.MinCost)
	require.NoError(t, err)
	admin, err := userRepo.Create(context.Background(), identitysqlc.CreateUserParams{
		Email:        "admin@test.com",
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         "Integration Admin",
	})
	require.NoError(t, err)
	admin, err = userRepo.UpdateRole(context.Background(), identitysqlc.UpdateUserRoleParams{
		ID:   admin.ID,
		Role: identitydto.RoleAdmin,
	})
	require.NoError(t, err)
	adminToken, err := token.GenerateWithVersionAndTenant(admin.ID, 1, admin.Email, admin.Role, admin.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)

	// Get stats.
	req, _ = http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// List users.
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

	// Unban user (restore).
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
	restoredToken, err := token.GenerateWithVersionAndTenant(restored.ID, 1, restored.Email, restored.Role, restored.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)
	req, _ = http.NewRequest("GET", "/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+restoredToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// Update role after unbanning, otherwise the last-admin guard would (by
	// design) reject banning the only active admin.
	body, _ = json.Marshal(admindto.UpdateRoleRequest{Role: "admin"})
	req, _ = http.NewRequest("PUT", "/admin/users/1/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	// A separate regular user gets 403 (not 401): their session is valid, but
	// their role is insufficient.
	body, _ = json.Marshal(identitydto.RegisterRequest{
		Email:    "other@test.com",
		Password: "Password1!",
		Name:     "Other User",
	})
	req, _ = http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode)

	var other identitydto.UserResponse
	respBody, _ = io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(respBody, &other))
	userToken, err := token.GenerateWithVersionAndTenant(other.ID, 1, other.Email, other.Role, other.TokenVersion, "integration-secret", 24)
	require.NoError(t, err)

	req, _ = http.NewRequest("GET", "/admin/stats", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}
