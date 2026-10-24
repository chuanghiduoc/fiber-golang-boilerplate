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

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const integrationTenantSecret = "tenant-integration-secret"

type tenantHarness struct {
	app       *fiber.App
	pool      *pgxpool.Pool
	userRepo  identityrepo.UserRepository
	adminSvc  identityservice.AdminUserService
	tenantOne int64
	tenantTwo int64
}

func setupTenantHarness(t *testing.T) *tenantHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var tenantTwo int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('acme', 'Acme') RETURNING id`,
	).Scan(&tenantTwo))

	userRepo := identityrepo.NewUserRepository(pool)
	tenantRepo := identityrepo.NewTenantRepository(pool)
	resolver := identityadapter.NewTenantResolver(tenantRepo)
	refreshRepo := identityrepo.NewRefreshTokenRepository(pool)
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := database.NewTxManager(pool)
	auditRecorder := audit.NewPostgresRecorder(pool)
	sessionStore := session.NewStore(userRepo, appCache)

	userSvc := identityservice.NewUserService(userRepo, refreshRepo, false, appCache, txManager, auditRecorder, sessionStore)
	refreshSvc := identityservice.NewRefreshTokenService(refreshRepo, 7)
	authHandler := identityhandler.NewAuthHandler(
		userSvc, refreshSvc, nil, nil,
		integrationTenantSecret, 24, false, nil, tenantRepo,
	)
	userHandler := identityhandler.NewUserHandler(userSvc)
	adminSvc := identityservice.NewAdminUserService(userRepo, refreshRepo, txManager, auditRecorder, sessionStore)

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	api := app.Group("/api/v1", tenant.Middleware(resolver, "default"))
	api.Post("/auth/register", authHandler.Register)
	api.Post("/auth/login", authHandler.Login)
	api.Post("/auth/refresh", authHandler.Refresh)

	users := api.Group("/users", middleware.JWTAuth(integrationTenantSecret, sessionStore))
	users.Get("/me", userHandler.GetMe)
	users.Get("/:id", userHandler.GetByID)
	users.Put("/:id", userHandler.Update)
	users.Delete("/:id", userHandler.Delete)
	users.Get("/", middleware.RequireRole("admin"), userHandler.List)

	return &tenantHarness{
		app:       app,
		pool:      pool,
		userRepo:  userRepo,
		adminSvc:  adminSvc,
		tenantOne: tenant.DefaultID,
		tenantTwo: tenantTwo,
	}
}

func tenantRequest(t *testing.T, app *fiber.App, method, path, slug string, body []byte, bearer string) (resp *http.Response, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if slug != "" {
		req.Header.Set(tenant.Header, slug)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err = app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func registerTenantUser(t *testing.T, h *tenantHarness, slug, email, password, name string) identitydto.UserResponse {
	t.Helper()
	body, err := json.Marshal(identitydto.RegisterRequest{Email: email, Password: password, Name: name})
	require.NoError(t, err)
	resp, respBody := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/register", slug, body, "")
	require.Equal(t, fiber.StatusCreated, resp.StatusCode, "body: %s", respBody)
	var user identitydto.UserResponse
	require.NoError(t, json.Unmarshal(respBody, &user))
	return user
}

func loginTenantUser(t *testing.T, h *tenantHarness, slug, email, password string) identitydto.LoginResponse {
	t.Helper()
	body, err := json.Marshal(identitydto.LoginRequest{Email: email, Password: password})
	require.NoError(t, err)
	resp, respBody := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/login", slug, body, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	var login identitydto.LoginResponse
	require.NoError(t, json.Unmarshal(respBody, &login))
	return login
}

func TestTenantIdentity_RegistrationLoginAndTokenClaim(t *testing.T) {
	h := setupTenantHarness(t)

	// The same address is a different account in each tenant.
	userA := registerTenantUser(t, h, "default", "shared@example.com", "Password1!", "Tenant One")
	userB := registerTenantUser(t, h, "acme", "shared@example.com", "Password2@", "Tenant Two")
	assert.Equal(t, h.tenantOne, userA.TenantID)
	assert.Equal(t, h.tenantTwo, userB.TenantID)
	assert.NotEqual(t, userA.ID, userB.ID)

	// A header-less login uses the default tenant.
	loginA := loginTenantUser(t, h, "", "shared@example.com", "Password1!")
	claimsA, err := token.Parse(loginA.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantOne, claimsA.TenantID)
	assert.Equal(t, h.tenantOne, loginA.User.TenantID)

	loginB := loginTenantUser(t, h, "acme", "shared@example.com", "Password2@")
	claimsB, err := token.Parse(loginB.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantTwo, claimsB.TenantID)
	assert.Equal(t, h.tenantTwo, loginB.User.TenantID)

	// Tenant one's password is not valid for tenant two's account.
	body, err := json.Marshal(identitydto.LoginRequest{Email: "shared@example.com", Password: "Password1!"})
	require.NoError(t, err)
	resp, _ := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/login", "acme", body, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// Repository lookups are scoped as well.
	got, err := h.userRepo.GetByEmail(tenant.WithID(context.Background(), h.tenantTwo), "shared@example.com")
	require.NoError(t, err)
	assert.Equal(t, userB.ID, got.ID)

	_, err = h.userRepo.GetByID(tenant.WithID(context.Background(), h.tenantTwo), userA.ID)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

func TestTenantIdentity_CrossTenantUserAccess(t *testing.T) {
	h := setupTenantHarness(t)

	adminA := registerTenantUser(t, h, "default", "admin-a@example.com", "AdminPass1!", "Admin A")
	userB := registerTenantUser(t, h, "acme", "user-b@example.com", "UserPass1!", "User B")

	_, err := h.userRepo.UpdateRole(tenant.WithID(context.Background(), h.tenantOne), identitysqlc.UpdateUserRoleParams{
		ID: adminA.ID, Role: identitydto.RoleAdmin,
	})
	require.NoError(t, err)
	loginAdmin := loginTenantUser(t, h, "default", "admin-a@example.com", "AdminPass1!")

	// A tenant-one admin cannot read, update, or delete a tenant-two user.
	pathB := "/api/v1/users/" + strconv.FormatInt(userB.ID, 10)
	resp, _ := tenantRequest(t, h.app, http.MethodGet, pathB, "", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	nameBody, _ := json.Marshal(identitydto.UpdateUserRequest{Name: stringPtr("Hijacked")})
	resp, _ = tenantRequest(t, h.app, http.MethodPut, pathB, "", nameBody, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	resp, _ = tenantRequest(t, h.app, http.MethodDelete, pathB, "", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// Listing only returns the admin's own tenant.
	resp, listBody := tenantRequest(t, h.app, http.MethodGet, "/api/v1/users", "", nil, loginAdmin.AccessToken)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", listBody)
	var page struct {
		Data []identitydto.UserResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(listBody, &page))
	require.Len(t, page.Data, 1)
	assert.Equal(t, adminA.ID, page.Data[0].ID)
	assert.Equal(t, h.tenantOne, page.Data[0].TenantID)

	// An explicit header from another tenant conflicts with the JWT claim.
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/users/me", "acme", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	// Tenant two's admin count is independent of tenant one.
	_, err = h.userRepo.UpdateRole(tenant.WithID(context.Background(), h.tenantTwo), identitysqlc.UpdateUserRoleParams{
		ID: userB.ID, Role: identitydto.RoleAdmin,
	})
	require.NoError(t, err)
	countA, err := h.userRepo.CountActiveAdmins(tenant.WithID(context.Background(), h.tenantOne))
	require.NoError(t, err)
	countB, err := h.userRepo.CountActiveAdmins(tenant.WithID(context.Background(), h.tenantTwo))
	require.NoError(t, err)
	assert.Equal(t, int64(1), countA)
	assert.Equal(t, int64(1), countB)

	// The database trigger also counts per tenant: tenant two's admin cannot
	// satisfy tenant one's last-admin invariant.
	_, err = h.userRepo.UpdateRole(tenant.WithID(context.Background(), h.tenantOne), identitysqlc.UpdateUserRoleParams{
		ID: adminA.ID, Role: identitydto.RoleUser,
	})
	require.Error(t, err, "demoting the only tenant-one admin must fail despite tenant two's admin")

	statsA, err := h.adminSvc.GetUserStats(tenant.WithID(context.Background(), h.tenantOne))
	require.NoError(t, err)
	statsB, err := h.adminSvc.GetUserStats(tenant.WithID(context.Background(), h.tenantTwo))
	require.NoError(t, err)
	assert.Equal(t, int64(1), statsA.ActiveUsers)
	assert.Equal(t, int64(1), statsB.ActiveUsers)
}

func TestTenantIdentity_RefreshTokenTenantMismatch(t *testing.T) {
	h := setupTenantHarness(t)
	registerTenantUser(t, h, "acme", "refresh@example.com", "Password1!", "Refresh User")
	login := loginTenantUser(t, h, "acme", "refresh@example.com", "Password1!")

	body, err := json.Marshal(identitydto.RefreshRequest{RefreshToken: login.RefreshToken})
	require.NoError(t, err)

	// The opaque refresh token is scoped to the tenant that issued it.
	resp, _ := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/refresh", "", body, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
	resp, _ = tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/refresh", "default", body, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	resp, respBody := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/refresh", "acme", body, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", respBody)
	var refreshed identitydto.LoginResponse
	require.NoError(t, json.Unmarshal(respBody, &refreshed))
	claims, err := token.Parse(refreshed.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantTwo, claims.TenantID)
}

func TestTenantIdentity_SuspendedTenantAndLegacyToken(t *testing.T) {
	h := setupTenantHarness(t)
	registerTenantUser(t, h, "acme", "suspended@example.com", "Password1!", "Suspended User")
	login := loginTenantUser(t, h, "acme", "suspended@example.com", "Password1!")

	// A legacy token without tenant_id is rejected fail-closed.
	legacy, err := token.Generate(login.User.ID, login.User.Email, login.User.Role, integrationTenantSecret, 24)
	require.NoError(t, err)
	resp, _ := tenantRequest(t, h.app, http.MethodGet, "/api/v1/users/me", "", nil, legacy)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	_, err = h.userRepo.UpdateRole(tenant.WithID(context.Background(), h.tenantTwo), identitysqlc.UpdateUserRoleParams{
		ID: login.User.ID, Role: identitydto.RoleAdmin,
	})
	require.NoError(t, err)

	// Suspend the tenant after a token was issued.
	_, err = h.userRepo.GetByID(tenant.WithID(context.Background(), h.tenantTwo), login.User.ID)
	require.NoError(t, err)
	h.suspendTenant(t, "acme")

	// The middleware refuses to resolve a suspended tenant for public auth.
	body, _ := json.Marshal(identitydto.LoginRequest{Email: "suspended@example.com", Password: "Password1!"})
	resp, _ = tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/login", "acme", body, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// An already-issued access token is rejected by the session validator even
	// when the header would otherwise resolve the default tenant; with the
	// suspended slug the middleware already returns 404.
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/users/me", "acme", nil, login.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/users/me", "", nil, login.AccessToken)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func (h *tenantHarness) suspendTenant(t *testing.T, slug string) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(),
		`UPDATE tenants SET status = 'suspended' WHERE slug = $1`, slug,
	)
	require.NoError(t, err)
}

func stringPtr(v string) *string { return &v }
