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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const integrationTenantSecret = "integration-app-secret-0123456789"

type tenantHarness struct {
	app       *fiber.App
	pool      *pgxpool.Pool
	stores    core.Stores
	adminSvc  membership.AdminUserService
	tenantOne uuid.UUID
	tenantTwo uuid.UUID
}

func setupTenantHarness(t *testing.T) *tenantHarness {
	t.Helper()
	ctx := context.Background()
	full := newIntegrationApp(t, "test")
	tenantTwo := testutil.CreateTenant(t, ctx, full.pool, "acme")
	stores := identityrepo.NewStores(full.fx.Scoped())
	return &tenantHarness{
		app:       full.app,
		pool:      full.pool,
		stores:    stores,
		adminSvc:  membership.NewAdminUserService(stores, nil, nil),
		tenantOne: tenant.DefaultID,
		tenantTwo: tenantTwo,
	}
}

func tenantRequest(t *testing.T, app *fiber.App, method, path, slug string, body []byte, bearer string) (resp *http.Response, respBody []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if body != nil {
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/merge-patch+json"
		}
		req.Header.Set("Content-Type", contentType)
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

func (h *tenantHarness) tenantCtx(id uuid.UUID) context.Context {
	return tenant.WithID(context.Background(), id)
}

func TestTenantIdentity_RegistrationLoginAndTokenClaim(t *testing.T) {
	h := setupTenantHarness(t)

	userA := registerTenantUser(t, h, "default", "shared@example.com", testPassphrase, "Tenant One")
	assert.Equal(t, h.tenantOne, userA.TenantID)

	// One global account per address: registering it again from another
	// tenant is a conflict, not a second account.
	body, err := json.Marshal(identitydto.RegisterRequest{Email: "Shared@Example.com", Password: testPassphrase, Name: "Tenant Two"})
	require.NoError(t, err)
	resp, respBody := tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/register", "acme", body, "")
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode, "body: %s", respBody)
	assert.Contains(t, string(respBody), "user.email_taken")

	// An account without a membership in a tenant cannot sign in there.
	body, err = json.Marshal(identitydto.LoginRequest{Email: "shared@example.com", Password: testPassphrase})
	require.NoError(t, err)
	resp, _ = tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/login", "acme", body, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// A header-less login uses the default tenant.
	loginA := loginTenantUser(t, h, "", "shared@example.com", testPassphrase)
	claimsA, err := token.Parse(loginA.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantOne, claimsA.TenantID)
	assert.Equal(t, h.tenantOne, loginA.User.TenantID)

	// Joining tenant two adds a membership to the same global user, who signs
	// in there with the same (global) password and gets a tenant-two token.
	testutil.CreateMember(t, context.Background(), h.pool, h.tenantTwo, "shared@example.com", testutil.MemberOptions{})
	loginB := loginTenantUser(t, h, "acme", "shared@example.com", testPassphrase)
	claimsB, err := token.Parse(loginB.AccessToken, integrationTenantSecret)
	require.NoError(t, err)
	assert.Equal(t, h.tenantTwo, claimsB.TenantID)
	assert.Equal(t, h.tenantTwo, loginB.User.TenantID)
	assert.Equal(t, userA.ID, loginB.User.ID)
	assert.NotEqual(t, claimsA.SessionID, claimsB.SessionID, "each tenant sign-in is its own session")

	// Membership lookups are tenant scoped.
	solo := registerTenantUser(t, h, "default", "solo@example.com", testPassphrase, "Solo")
	_, err = h.stores.Memberships.Get(h.tenantCtx(h.tenantTwo), solo.ID)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
	member, err := h.stores.Memberships.Get(h.tenantCtx(h.tenantTwo), userA.ID)
	require.NoError(t, err)
	assert.Equal(t, h.tenantTwo, member.Membership.TenantID)
}

func TestTenantIdentity_CrossTenantUserAccess(t *testing.T) {
	h := setupTenantHarness(t)

	adminA := registerTenantUser(t, h, "default", "admin-a@example.com", testPassphrase, "Admin A")
	userB := registerTenantUser(t, h, "acme", "user-b@example.com", testPassphrase, "User B")

	_, err := h.stores.Memberships.UpdateRole(h.tenantCtx(h.tenantOne), adminA.ID, identitydto.RoleOwner)
	require.NoError(t, err)
	_, err = h.stores.Memberships.UpdateRole(h.tenantCtx(h.tenantTwo), userB.ID, identitydto.RoleOwner)
	require.NoError(t, err)
	loginAdmin := loginTenantUser(t, h, "default", "admin-a@example.com", testPassphrase)

	// A tenant-one admin cannot read or remove a tenant-two user: the user has
	// no membership here, which is not found (not forbidden).
	pathB := "/api/v1/members/" + userB.ID.String()
	resp, _ := tenantRequest(t, h.app, http.MethodGet, pathB, "", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	resp, _ = tenantRequest(t, h.app, http.MethodDelete, pathB, "", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// Listing only returns the admin's own tenant.
	resp, listBody := tenantRequest(t, h.app, http.MethodGet, "/api/v1/members", "", nil, loginAdmin.AccessToken)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", listBody)
	var page struct {
		Data []identitydto.UserResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(listBody, &page))
	require.Len(t, page.Data, 1)
	assert.Equal(t, adminA.ID, page.Data[0].ID)
	assert.Equal(t, h.tenantOne, page.Data[0].TenantID)

	// The profile is the caller's own; there is no route to edit another user.
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/me/profile", "", nil, loginAdmin.AccessToken)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/me/profile", "acme", nil, loginAdmin.AccessToken)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode,
		"an explicit header from another tenant conflicts with the JWT claim")

	// Tenant two's owner count is independent of tenant one.
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
	registerTenantUser(t, h, "acme", "refresh@example.com", testPassphrase, "Refresh User")
	login := loginTenantUser(t, h, "acme", "refresh@example.com", testPassphrase)

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
	member := testutil.CreateMember(t, context.Background(), h.pool, h.tenantTwo, "suspended@example.com",
		testutil.MemberOptions{Password: testPassphrase})
	login := identitydto.LoginResponse{
		User:        identitydto.UserResponse{ID: member.UserID, TenantID: member.TenantID},
		AccessToken: testutil.SessionToken(t, context.Background(), h.pool, member, integrationTenantSecret),
	}

	// A legacy token without tenant_id is rejected fail-closed.
	legacy, err := token.Generate(login.User.ID, integrationTenantSecret, 24*time.Hour)
	require.NoError(t, err)
	resp, _ := tenantRequest(t, h.app, http.MethodGet, "/api/v1/me/profile", "", nil, legacy)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	// A tenant-bound token without a session id is rejected too.
	sessionless, err := token.GenerateWithVersionAndTenant(login.User.ID, h.tenantTwo, 0, integrationTenantSecret, 24*time.Hour)
	require.NoError(t, err)
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/me/profile", "", nil, sessionless)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	h.suspendTenant(t, "acme")

	// The middleware refuses to resolve a suspended tenant for public auth.
	body, _ := json.Marshal(identitydto.LoginRequest{Email: "suspended@example.com", Password: testPassphrase})
	resp, _ = tenantRequest(t, h.app, http.MethodPost, "/api/v1/auth/login", "acme", body, "")
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// An already-issued access token is rejected by the session validator even
	// when the header would otherwise resolve the default tenant; with the
	// suspended slug the middleware already returns 404.
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/me/profile", "acme", nil, login.AccessToken)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	resp, _ = tenantRequest(t, h.app, http.MethodGet, "/api/v1/me/profile", "", nil, login.AccessToken)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)
}

func (h *tenantHarness) suspendTenant(t *testing.T, slug string) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(),
		`UPDATE tenants SET status = 'suspended' WHERE slug = $1`, slug,
	)
	require.NoError(t, err)
}
