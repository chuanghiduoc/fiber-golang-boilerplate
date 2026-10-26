//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	adminservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
	filesadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const adminIntegrationSecret = "integration-secret"

func setupIntegrationApp(t *testing.T) (*fiber.App, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := testutil.NewTxManager(pool)
	auditRecorder := audit.NewPostgresRecorder(testutil.Scoped(pool))
	kit := newIdentityKit(pool, appCache, auditRecorder, adminIntegrationSecret, nil)

	fileRepo := filesrepo.NewFileRepository(testutil.Scoped(pool))
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	identityAdmin := identityadapter.NewAdminIdentity(kit.admin)
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
		// This minimal app has no tenant middleware: the route is the default
		// tenant by construction, and the RLS resolver is fail-closed.
		ctx := tenant.WithID(c.Context(), tenant.DefaultID)
		user, err := kit.users.Register(ctx, identityservice.RegisterInput{
			Email:    req.Email,
			Password: req.Password,
			Name:     req.Name,
		})
		if err != nil {
			return err
		}
		return response.Created(c, identitydto.UserResponse{
			ID:       user.User.ID,
			TenantID: user.Membership.TenantID,
			Email:    user.User.Email,
			Name:     user.User.Name,
			Role:     user.Membership.Role,
		})
	})

	users := app.Group("/users", middleware.JWTAuth(adminIntegrationSecret, kit.sessions))
	users.Get("/me", kit.userHandler.GetMe)

	admin := app.Group("/admin",
		middleware.JWTAuth(adminIntegrationSecret, kit.sessions),
		middleware.RequireRole("admin"),
	)
	admin.Get("/stats", adminHandler.GetStats)
	admin.Get("/users", adminHandler.ListUsers)
	admin.Put("/users/:id/role", adminHandler.UpdateRole)
	admin.Post("/users/:id/ban", adminHandler.BanUser)
	admin.Post("/users/:id/unban", adminHandler.UnbanUser)

	return app, pool
}

// registerMember registers through the HTTP route and returns the new member
// of the default tenant (the route has no tenant middleware).
func registerMember(t *testing.T, app *fiber.App, email, name string) testutil.Member {
	t.Helper()
	body, err := json.Marshal(identitydto.RegisterRequest{Email: email, Password: testPassphrase, Name: name})
	require.NoError(t, err)
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	respBody, _ := io.ReadAll(resp.Body)
	require.Equal(t, fiber.StatusCreated, resp.StatusCode, "body: %s", respBody)
	var user identitydto.UserResponse
	require.NoError(t, json.Unmarshal(respBody, &user))
	return testutil.Member{UserID: user.ID, TenantID: tenant.DefaultID, Email: user.Email, Role: user.Role}
}

func TestIntegration_AdminOperations(t *testing.T) {
	app, pool := setupIntegrationApp(t)
	ctx := context.Background()

	do := func(method, path, bearer string, body []byte) int {
		t.Helper()
		var reader io.Reader = http.NoBody
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, _ := http.NewRequest(method, path, reader)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
		require.NoError(t, err)
		return resp.StatusCode
	}

	regular := registerMember(t, app, "regular@test.com", "Regular User")
	regularToken := testutil.SessionToken(t, ctx, pool, regular, adminIntegrationSecret)

	// A real admin membership with a live session, so the session validator
	// has a current role/version to compare against.
	admin := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, "admin@test.com",
		testutil.MemberOptions{Role: identitydto.RoleAdmin})
	adminToken := testutil.SessionToken(t, ctx, pool, admin, adminIntegrationSecret)

	assert.Equal(t, fiber.StatusOK, do("GET", "/admin/stats", adminToken, nil))
	assert.Equal(t, fiber.StatusOK, do("GET", "/admin/users", adminToken, nil))

	// Ban the membership while it is still a regular user.
	regularPath := "/admin/users/" + strconv.FormatInt(regular.UserID, 10)
	assert.Equal(t, fiber.StatusNoContent, do("POST", regularPath+"/ban", adminToken, nil))

	// A pre-ban access token stops working immediately: the ban revoked the
	// member's sessions in this tenant.
	assert.Equal(t, fiber.StatusUnauthorized, do("GET", "/users/me", regularToken, nil))

	assert.Equal(t, fiber.StatusOK, do("POST", regularPath+"/unban", adminToken, nil))

	// The pre-ban session stays revoked after the unban.
	assert.Equal(t, fiber.StatusUnauthorized, do("GET", "/users/me", regularToken, nil))

	// A new session for the restored member works.
	restoredToken := testutil.SessionToken(t, ctx, pool, regular, adminIntegrationSecret)
	assert.Equal(t, fiber.StatusOK, do("GET", "/users/me", restoredToken, nil))

	// Promote after unbanning, otherwise the last-owner guard would (by
	// design) reject banning the only active admin.
	body, _ := json.Marshal(admindto.UpdateRoleRequest{Role: "admin"})
	assert.Equal(t, fiber.StatusOK, do("PUT", regularPath+"/role", adminToken, body))

	// A separate regular user gets 403 (not 401): their session is valid, but
	// their role is insufficient.
	other := registerMember(t, app, "other@test.com", "Other User")
	userToken := testutil.SessionToken(t, ctx, pool, other, adminIntegrationSecret)
	assert.Equal(t, fiber.StatusForbidden, do("GET", "/admin/stats", userToken, nil))
}
