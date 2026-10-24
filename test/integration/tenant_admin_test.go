//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgtype"
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
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const tenantAdminSecret = "tenant-admin-integration-secret"

type tenantAdminHarness struct {
	app        *fiber.App
	pool       *pgxpool.Pool
	userRepo   identityrepo.UserRepository
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

	var tenantB int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('admin-acme', 'Admin Acme') RETURNING id`,
	).Scan(&tenantB))

	insertUser := func(tenantID int64, email, role string, deleted bool) int64 {
		t.Helper()
		var deletedAt pgtype.Timestamptz
		if deleted {
			deletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO users (tenant_id, email, name, role, deleted_at)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			tenantID, email, strings.Split(email, "@")[0], role, deletedAt,
		).Scan(&id))
		return id
	}

	adminAID := insertUser(tenant.DefaultID, "admin-a@example.com", identitydto.RoleAdmin, false)
	insertUser(tenant.DefaultID, "user-a@example.com", identitydto.RoleUser, false)
	userBID := insertUser(tenantB, "user-b@example.com", identitydto.RoleUser, false)
	bannedBID := insertUser(tenantB, "banned-b@example.com", identitydto.RoleUser, true)

	adminToken, err := token.GenerateWithVersionAndTenant(
		adminAID, tenant.DefaultID, "admin-a@example.com", identitydto.RoleAdmin, 0,
		tenantAdminSecret, 24,
	)
	require.NoError(t, err)

	userRepo := identityrepo.NewUserRepository(pool)
	tenantRepo := identityrepo.NewTenantRepository(pool)
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := database.NewTxManager(pool)
	sessionStore := session.NewStore(userRepo, appCache)
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)

	identityAdmin := identityadapter.NewAdminIdentity(
		identityservice.NewAdminUserService(userRepo, identityrepo.NewRefreshTokenRepository(pool), txManager, sessionStore),
	)
	filesRepo := filesrepo.NewFileRepository(pool)
	filesAdmin := filesadapter.NewAdminFiles(filesvc.NewAdminFileService(filesRepo, store))
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
		Sessions:       sessionStore,
		TenantResolver: identityadapter.NewTenantResolver(tenantRepo),
	})
	application.Register(admin.New(adminHandler))

	return &tenantAdminHarness{
		app:        application.Fiber(),
		pool:       pool,
		userRepo:   userRepo,
		adminToken: adminToken,
		tenantB:    tenantB,
		adminAID:   adminAID,
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

func TestTenantAdminModule_CrossTenantOperationsAreNotFound(t *testing.T) {
	h := setupTenantAdminHarness(t)

	// List: only tenant A accounts are visible.
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

	// Stats: tenant A's two active users only.
	status, body = h.request(t, http.MethodGet, "/api/v1/admin/stats", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var stats admindto.AdminStatsResponse
	require.NoError(t, json.Unmarshal(body, &stats))
	assert.Equal(t, int64(2), stats.ActiveUsers)
	assert.Zero(t, stats.DeletedUsers)

	// Role update for a tenant B user id: not found, not forbidden.
	status, body = h.request(t, http.MethodPut,
		"/api/v1/admin/users/"+strconv.FormatInt(h.userBID, 10)+"/role", `{"role":"admin"}`)
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	// Ban/unban a tenant B user id: not found, and rows are unchanged.
	status, body = h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(h.userBID, 10)+"/ban", "")
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	status, body = h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(h.bannedBID, 10)+"/unban", "")
	assert.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	userB, err := h.userRepo.GetByID(tenant.WithID(context.Background(), h.tenantB), h.userBID)
	require.NoError(t, err)
	assert.Equal(t, identitydto.RoleUser, userB.Role)
	assert.False(t, userB.DeletedAt.Valid, "tenant B active user must not be banned")

	var bannedRole string
	var bannedDeleted bool
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT role, deleted_at IS NOT NULL FROM users WHERE id = $1`, h.bannedBID,
	).Scan(&bannedRole, &bannedDeleted))
	assert.Equal(t, identitydto.RoleUser, bannedRole)
	assert.True(t, bannedDeleted, "tenant B banned user must stay banned")

	// The tenant A admin still exists and was not affected by the attempts.
	adminA, err := h.userRepo.GetByID(context.Background(), h.adminAID)
	require.NoError(t, err)
	assert.Equal(t, identitydto.RoleAdmin, adminA.Role)
}
