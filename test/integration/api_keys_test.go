//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apikeyadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/adapter"
	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	apikeyservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey/apikeyhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

const apiKeyIntegrationSecret = "api-key-integration-secret"

type apiKeyHarness struct {
	app    *fiber.App
	pool   *pgxpool.Pool
	repo   apikeyrepo.APIKeyRepository
	svc    apikeyservice.Service
	tenant int64
	other  int64
	user   int64
	userB  int64
}

// systemTenantCtx is a trusted system-caller context for tenantID; these
// tests drive the API-key service directly, without an HTTP subject.
func systemTenantCtx(tenantID int64) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), tenantID))
}

func setupAPIKeyHarness(t *testing.T) *apiKeyHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantB := testutil.CreateTenant(t, ctx, pool, "apikey-acme")

	insertUser := func(tenantID int64, email string) int64 {
		t.Helper()
		return testutil.CreateMember(t, ctx, pool, tenantID, email, testutil.MemberOptions{Role: "admin"}).UserID
	}
	userA := insertUser(tenant.DefaultID, "apikey-a@test.com")
	userB := insertUser(tenantB, "apikey-b@test.com")

	stores := identityrepo.NewStores(pool)
	apiKeyRepo := apikeyrepo.NewAPIKeyRepository(pool)
	principalReader := identityadapter.NewPrincipalReader(stores.Memberships)
	verifier := apikey.NewVerifier(apikeyadapter.NewVerifierStore(apiKeyRepo), principalReader)
	svc := apikeyservice.NewService(apiKeyRepo, principalReader, nil, audit.Noop{})

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := app.Group("/api/v1", tenanthttp.Middleware(
		identityadapter.NewTenantResolver(stores.Tenants),
		tenant.DefaultSlug,
	))
	protected := api.Group("/protected", apikeyhttp.Authenticate(
		middleware.JWTAuth(apiKeyIntegrationSecret),
		verifier,
	))
	protected.Get("/", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"userID":     fiber.Locals[int64](c, "user_id"),
			"tenantID":   fiber.Locals[int64](c, "tenant_id"),
			"apiKeyID":   fiber.Locals[int64](c, apikey.LocalAPIKeyID),
			"authMethod": fiber.Locals[string](c, apikey.LocalAuthMethod),
		})
	})

	return &apiKeyHarness{
		app:    app,
		pool:   pool,
		repo:   apiKeyRepo,
		svc:    svc,
		tenant: tenant.DefaultID,
		other:  tenantB,
		user:   userA,
		userB:  userB,
	}
}

func (h *apiKeyHarness) request(t *testing.T, key, slug string) (res *http.Response, payload []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/api/v1/protected/", http.NoBody)
	require.NoError(t, err)
	req.Header.Set(fiber.HeaderAuthorization, apikey.APIKeyScheme+" "+key)
	if slug != "" {
		req.Header.Set(tenant.Header, slug)
	}
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func TestIntegration_APIKeyAuthenticationLifecycle(t *testing.T) {
	h := setupAPIKeyHarness(t)

	created, err := h.svc.Create(systemTenantCtx(h.tenant), h.user, apikeydto.CreateRequest{
		Name:   "integration",
		Scopes: []string{"files:read"},
	})
	require.NoError(t, err)

	resp, body := h.request(t, created.Key, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var payload struct {
		UserID     int64  `json:"userID"`
		TenantID   int64  `json:"tenantID"`
		APIKeyID   int64  `json:"apiKeyID"`
		AuthMethod string `json:"authMethod"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	assert.Equal(t, h.user, payload.UserID)
	assert.Equal(t, h.tenant, payload.TenantID)
	assert.Equal(t, created.ID, payload.APIKeyID)
	assert.Equal(t, apikey.AuthMethodAPIKey, payload.AuthMethod)

	// The verifier records last use.
	var lastUsed *time.Time
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT last_used_at FROM api_keys WHERE id = $1 AND tenant_id = $2`,
		created.ID, h.tenant,
	).Scan(&lastUsed))
	assert.NotNil(t, lastUsed, "successful authentication must touch last_used_at")

	// An explicit conflicting tenant header is 403, even for a valid key.
	var otherSlug string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT slug FROM tenants WHERE id = $1`, h.other,
	).Scan(&otherSlug))
	resp, _ = h.request(t, created.Key, otherSlug)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode, "tenant mismatch")

	// Rotation keeps the old key valid during grace and issues a working new key.
	rotated, err := h.svc.Rotate(systemTenantCtx(h.tenant), created.ID)
	require.NoError(t, err)
	resp, _ = h.request(t, created.Key, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "old key within rotation grace")
	resp, _ = h.request(t, rotated.Key, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "new key after rotation")

	// Revocation is immediate.
	require.NoError(t, h.svc.Revoke(systemTenantCtx(h.tenant), rotated.ID))
	resp, _ = h.request(t, rotated.Key, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "revoked key")
}

func TestIntegration_APIKeyTenantBinding(t *testing.T) {
	h := setupAPIKeyHarness(t)

	keyB, err := h.svc.Create(systemTenantCtx(h.other), h.userB, apikeydto.CreateRequest{
		Name:   "tenant-b",
		Scopes: []string{"files:read"},
	})
	require.NoError(t, err)

	// No header: the key's tenant is authoritative, so a non-default key works.
	resp, body := h.request(t, keyB.Key, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var payload struct {
		TenantID int64 `json:"tenantID"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	assert.Equal(t, h.other, payload.TenantID)

	// An explicit default-tenant header conflicts with the key binding.
	resp, _ = h.request(t, keyB.Key, tenant.DefaultSlug)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}
