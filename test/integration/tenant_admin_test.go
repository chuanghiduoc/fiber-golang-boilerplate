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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin"
	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	adminservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
	filesadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const tenantAdminSecret = "tenant-admin-integration-secret"

type tenantAdminHarness struct {
	app        *fiber.App
	pool       *pgxpool.Pool
	adminToken string
	tenantB    int64
	adminAID   int64
	userBID    int64
	bannedBID  int64
}

func setupTenantAdminHarness(t *testing.T) *tenantAdminHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantB := testutil.CreateTenant(t, ctx, pool, "admin-acme")

	member := func(tenantID int64, email, role string) testutil.Member {
		t.Helper()
		return testutil.CreateMember(t, ctx, pool, tenantID, email, testutil.MemberOptions{Role: role})
	}
	adminA := member(tenant.DefaultID, "admin-a@example.com", identitydto.RoleAdmin)
	member(tenant.DefaultID, "user-a@example.com", identitydto.RoleUser)
	userBID := member(tenantB, "user-b@example.com", identitydto.RoleUser).UserID
	bannedBID := member(tenantB, "banned-b@example.com", identitydto.RoleUser).UserID
	_, err = pool.Exec(ctx,
		`UPDATE memberships SET status = 'suspended', banned_at = NOW() WHERE tenant_id = $1 AND user_id = $2`,
		tenantB, bannedBID)
	require.NoError(t, err)

	adminToken := testutil.SessionToken(t, ctx, pool, adminA, tenantAdminSecret)

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := database.NewTxManager(pool)
	auditRecorder := audit.NewPostgresRecorder(pool)
	kit := newIdentityKit(pool, appCache, auditRecorder, tenantAdminSecret, nil)
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	identityAdmin := identityadapter.NewAdminIdentity(kit.admin)
	filesRepo := filesrepo.NewFileRepository(pool)
	filesAdmin := filesadapter.NewAdminFiles(filesvc.NewAdminFileService(filesRepo, filesrepo.NewTransactor(txManager, auditRecorder, nil), store))
	adminHandler := adminhandler.NewHandler(adminservice.NewService(identityAdmin, filesAdmin))

	cfg := &config.Config{
		App: config.AppConfig{
			Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5,
		},
		CORS: config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{
			StrictMax: 1000, StrictWindow: 60,
			NormalMax: 1000, NormalWindow: 60,
			RelaxedMax: 1000, RelaxedWindow: 60,
		},
		JWT:    config.JWTConfig{Secret: tenantAdminSecret},
		Tenant: config.TenantConfig{DefaultSlug: "default", RequireHeader: true},
	}
	application := app.New(cfg, app.Deps{
		Cache:          appCache,
		Sessions:       kit.sessions,
		TenantResolver: identityadapter.NewTenantResolver(kit.stores.Tenants),
	})
	application.Register(admin.New(adminHandler))

	return &tenantAdminHarness{
		app:        application.Fiber(),
		pool:       pool,
		adminToken: adminToken,
		tenantB:    tenantB,
		adminAID:   adminA.UserID,
		userBID:    userBID,
		bannedBID:  bannedBID,
	}
}

func (h *tenantAdminHarness) request(t *testing.T, method, path, body string) (status int, respBody []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	// Production mode requires an explicit tenant; the token belongs to A.
	req.Header.Set(tenant.Header, "default")
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, respBody
}

// membership reads a user's membership role and status in tenantID.
func (h *tenantAdminHarness) membership(t *testing.T, tenantID, userID int64) (role, status string) {
	t.Helper()
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT role, status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID,
	).Scan(&role, &status))
	return role, status
}

func TestTenantAdminModule_CrossTenantOperationsAreNotFound(t *testing.T) {
	h := setupTenantAdminHarness(t)

	// List: only tenant A members are visible.
	status, body := h.request(t, http.MethodGet, "/api/v1/admin/users", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var page struct {
		Data []admindto.UserResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Data, 2)
	for _, user := range page.Data {
		assert.NotEqual(t, h.userBID, user.ID)
		assert.NotEqual(t, h.bannedBID, user.ID)
	}

	// Stats: tenant A's two active members only.
	status, body = h.request(t, http.MethodGet, "/api/v1/admin/stats", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var stats admindto.AdminStatsResponse
	require.NoError(t, json.Unmarshal(body, &stats))
	assert.Equal(t, int64(2), stats.ActiveUsers)
	assert.Zero(t, stats.DeletedUsers)

	// Role update for a user without a tenant A membership: not found, not
	// forbidden.
	status, body = h.request(t, http.MethodPut,
		"/api/v1/admin/users/"+strconv.FormatInt(h.userBID, 10)+"/role", `{"role":"admin"}`)
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	// Ban/unban of tenant B members: not found, and memberships are unchanged.
	status, body = h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(h.userBID, 10)+"/ban", "")
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	status, body = h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(h.bannedBID, 10)+"/unban", "")
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	role, memberStatus := h.membership(t, h.tenantB, h.userBID)
	assert.Equal(t, identitydto.RoleUser, role)
	assert.Equal(t, "active", memberStatus, "tenant B active member must not be banned")

	role, memberStatus = h.membership(t, h.tenantB, h.bannedBID)
	assert.Equal(t, identitydto.RoleUser, role)
	assert.Equal(t, "suspended", memberStatus, "tenant B banned member must stay banned")

	// The tenant A admin still exists and was not affected by the attempts.
	role, memberStatus = h.membership(t, tenant.DefaultID, h.adminAID)
	assert.Equal(t, identitydto.RoleAdmin, role)
	assert.Equal(t, "active", memberStatus)
}
