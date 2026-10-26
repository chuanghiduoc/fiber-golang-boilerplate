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
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const integrationTenantSecret = "tenant-integration-secret"

type tenantHarness struct {
	app       *fiber.App
	pool      *pgxpool.Pool
	stores    identityservice.Stores
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

	tenantTwo := testutil.CreateTenant(t, ctx, pool, "acme")

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	kit := newIdentityKit(pool, appCache, audit.NewPostgresRecorder(testutil.Scoped(pool)), integrationTenantSecret, nil)

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := app.Group("/api/v1", tenanthttp.Middleware(identityadapter.NewTenantResolver(kit.stores.Tenants), "default"))
	api.Post("/auth/register", kit.auth.Register)
	api.Post("/auth/login", kit.auth.Login)
	api.Post("/auth/refresh", kit.auth.Refresh)

	users := api.Group("/users", middleware.JWTAuth(integrationTenantSecret, kit.sessions))
	users.Get("/me", kit.userHandler.GetMe)
	users.Get("/:id", kit.userHandler.GetByID)
	users.Put("/:id", kit.userHandler.Update)
	users.Delete("/:id", kit.userHandler.Delete)
	users.Get("/", authzhttp.Require(authz.PermissionAdminUsersRead), kit.userHandler.List)

	return &tenantHarness{
		app:       app,
		pool:      pool,
		stores:    kit.stores,
		adminSvc:  kit.admin,
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

func (h *tenantHarness) tenantCtx(id int64) context.Context {
	return tenant.WithID(context.Background(), id)
}

func TestTenantIdentity_RegistrationLoginAndTokenClaim(t *testing.T) {
	h := setupTenantHarness(t)

	userA := registerTenantUser(t, h, "default", "shared@example.com", "Password1!2026", "Tenant One")
	assert.Equal(t, h.tenantOne, userA.TenantID)

	// One global account per address: registering it again from another
	// tenant is a conflict, not a second account.
	body, err := json.Marshal(identitydto.RegisterRequest{Email: "Shared@Example.com", Password: "Password2!2026", Name: "Tenant Two"})
	require.NoError(t, err)
	resp, respBody := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/register", "acme", body, "")
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode, "body: %s", respBody)
	assert.Contains(t, string(respBody), "user.email_taken")

	// An account without a membership in a tenant cannot sign in there.
	body, err = json.Marshal(identitydto.LoginRequest{Email: "shared@example.com", Password: "Password1!2026"})
	require.NoError(t, err)
	resp, _ = tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/login", "acme", body, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// A header-less login uses the default tenant.
	loginA := loginTenantUser(t, h, "", "shared@example.com", "Password1!2026")
	claimsA, err := token.Parse(loginA.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantOne, claimsA.TenantID)
	assert.Equal(t, h.tenantOne, loginA.User.TenantID)

	// Joining tenant two adds a membership to the same global user, who signs
	// in there with the same (global) password and gets a tenant-two token.
	testutil.CreateMember(t, context.Background(), h.pool, h.tenantTwo, "shared@example.com", testutil.MemberOptions{})
	loginB := loginTenantUser(t, h, "acme", "shared@example.com", "Password1!2026")
	claimsB, err := token.Parse(loginB.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantTwo, claimsB.TenantID)
	assert.Equal(t, h.tenantTwo, loginB.User.TenantID)
	assert.Equal(t, userA.ID, loginB.User.ID)
	assert.NotEqual(t, claimsA.SessionID, claimsB.SessionID, "each tenant sign-in is its own session")

	// Membership lookups are tenant scoped.
	solo := registerTenantUser(t, h, "default", "solo@example.com", "Password1!2026", "Solo")
	_, err = h.stores.Memberships.Get(h.tenantCtx(h.tenantTwo), solo.ID)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
	member, err := h.stores.Memberships.Get(h.tenantCtx(h.tenantTwo), userA.ID)
	require.NoError(t, err)
	assert.Equal(t, h.tenantTwo, member.Membership.TenantID)
}

func TestTenantIdentity_CrossTenantUserAccess(t *testing.T) {
	h := setupTenantHarness(t)

	adminA := registerTenantUser(t, h, "default", "admin-a@example.com", "AdminPass1!2026", "Admin A")
	userB := registerTenantUser(t, h, "acme", "user-b@example.com", "UserPass1!2026", "User B")

	_, err := h.stores.Memberships.UpdateRole(h.tenantCtx(h.tenantOne), adminA.ID, identitydto.RoleOwner)
	require.NoError(t, err)
	loginAdmin := loginTenantUser(t, h, "default", "admin-a@example.com", "AdminPass1!2026")

	// A tenant-one admin cannot read or remove a tenant-two user: the user has
	// no membership here, which is not found (not forbidden).
	pathB := "/api/v1/users/" + strconv.FormatInt(userB.ID, 10)
	resp, _ := tenantRequest(t, h.app, http.MethodGet, pathB, "", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	resp, _ = tenantRequest(t, h.app, http.MethodDelete, pathB, "", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// Profiles are global and only editable by their owner, so another
	// user's profile is forbidden for every caller, including admins.
	nameBody, _ := json.Marshal(identitydto.UpdateUserRequest{Name: stringPtr("Hijacked")})
	resp, respBody := tenantRequest(t, h.app, http.MethodPut, pathB, "", nameBody, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(respBody), string(apperror.CodePermissionDenied))
	var name string
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT name FROM users WHERE id = $1`, userB.ID).Scan(&name))
	assert.Equal(t, "User B", name)

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

	// Tenant two's owner count is independent of tenant one.
	_, err = h.stores.Memberships.UpdateRole(h.tenantCtx(h.tenantTwo), userB.ID, identitydto.RoleOwner)
	require.NoError(t, err)
	countA, err := h.stores.Memberships.CountActiveOwners(h.tenantCtx(h.tenantOne))
	require.NoError(t, err)
	countB, err := h.stores.Memberships.CountActiveOwners(h.tenantCtx(h.tenantTwo))
	require.NoError(t, err)
	assert.Equal(t, int64(1), countA)
	assert.Equal(t, int64(1), countB)

	// The database trigger also counts per tenant: tenant two's owner cannot
	// satisfy tenant one's last-owner invariant.
	_, err = h.stores.Memberships.UpdateRole(h.tenantCtx(h.tenantOne), adminA.ID, identitydto.RoleMember)
	require.Error(t, err, "demoting the only tenant-one owner must fail despite tenant two's owner")

	statsA, err := h.adminSvc.GetUserStats(h.tenantCtx(h.tenantOne))
	require.NoError(t, err)
	statsB, err := h.adminSvc.GetUserStats(h.tenantCtx(h.tenantTwo))
	require.NoError(t, err)
	assert.Equal(t, int64(1), statsA.ActiveUsers)
	assert.Equal(t, int64(1), statsB.ActiveUsers)
}

func TestTenantIdentity_RefreshTokenTenantMismatch(t *testing.T) {
	h := setupTenantHarness(t)
	registerTenantUser(t, h, "acme", "refresh@example.com", "Password1!2026", "Refresh User")
	login := loginTenantUser(t, h, "acme", "refresh@example.com", "Password1!2026")

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
	registerTenantUser(t, h, "acme", "suspended@example.com", "Password1!2026", "Suspended User")
	login := loginTenantUser(t, h, "acme", "suspended@example.com", "Password1!2026")

	// A legacy token without tenant_id is rejected fail-closed.
	legacy, err := token.Generate(login.User.ID, login.User.Email, login.User.Role, integrationTenantSecret, 24)
	require.NoError(t, err)
	resp, _ := tenantRequest(t, h.app, http.MethodGet, "/api/v1/users/me", "", nil, legacy)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// A tenant-bound token without a session id is rejected too.
	sessionless, err := token.GenerateWithVersionAndTenant(login.User.ID, h.tenantTwo, login.User.Email,
		login.User.Role, 0, integrationTenantSecret, 24)
	require.NoError(t, err)
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/users/me", "acme", nil, sessionless)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// Suspend the tenant after the session was issued. (No request validates
	// the token first: the validator caches session snapshots, and this raw
	// UPDATE bypasses the identity service's cache invalidation.)
	h.suspendTenant(t, "acme")

	// The middleware refuses to resolve a suspended tenant for public auth.
	body, _ := json.Marshal(identitydto.LoginRequest{Email: "suspended@example.com", Password: "Password1!2026"})
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
