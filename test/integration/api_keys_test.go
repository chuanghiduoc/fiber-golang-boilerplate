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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apikeyadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/adapter"
	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	apikeyservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey/apikeyhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

const apiKeyIntegrationSecret = "api-key-integration-secret"

type apiKeyHarness struct {
	app    *fiber.App
	pool   *pgxpool.Pool
	svc    apikeyservice.Service
	tenant uuid.UUID
	other  uuid.UUID
	user   uuid.UUID
	userB  uuid.UUID
}

// systemTenantCtx is a trusted system-caller context for tenantID; these
// tests drive the API-key service directly, without an HTTP subject.
func systemTenantCtx(tenantID uuid.UUID) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), tenantID))
}

func setupAPIKeyHarness(t *testing.T) *apiKeyHarness {
	t.Helper()
	ctx := context.Background()

	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	tenantB := testutil.CreateTenant(t, ctx, pool, "apikey-acme")

	insertUser := func(tenantID uuid.UUID, email string) uuid.UUID {
		t.Helper()
		return testutil.CreateMember(t, ctx, pool, tenantID, email, testutil.MemberOptions{Role: "admin"}).UserID
	}
	userA := insertUser(tenant.DefaultID, "apikey-a@test.com")
	userB := insertUser(tenantB, "apikey-b@test.com")

	stores := identityrepo.NewStores(fx.Scoped())
	// Production wires every repository to the Scoped handle, so the verifier's
	// hash lookup and last-used update run under RLS exactly as in the API.
	apiKeyRepo := apikeyrepo.NewAPIKeyRepository(fx.Scoped())
	principalReader := identityadapter.NewPrincipalReader(stores.Memberships)
	verifier := apikey.NewVerifier(apikeyadapter.NewVerifierStore(apiKeyRepo), principalReader)
	txManager := fx.Tx()
	ranks := identityadapter.NewMemberRank(stores, identityrepo.NewTransactor(txManager, audit.Noop{}, nil, nil))
	svc := apikeyservice.NewService(apiKeyRepo, principalReader, ranks, apikeyrepo.NewTransactor(txManager, audit.Noop{}, principalReader), authz.NewAuthorizer(integrationCatalogs().Permissions), 8760*time.Hour)

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := app.Group("/api/v1", tenanthttp.Middleware(
		identityadapter.NewTenantResolver(stores.Tenants),
		tenant.DefaultSlug,
	))
	protected := api.Group("/protected", apikeyhttp.Authenticate(
		middleware.JWTAuth(apiKeyIntegrationSecret, apiKeyTestSessions{}),
		verifier,
		integrationCatalogs().Permissions,
	))
	protected.Get("/", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"userID":     requestctx.ActorID(c.Context()),
			"tenantID":   tenant.IDFromContext(c.Context()),
			"apiKeyID":   apikey.APIKeyIDFromContext(c.Context()),
			"authMethod": apikey.AuthMethodFromContext(c.Context()),
		})
	})

	return &apiKeyHarness{
		app:    app,
		pool:   pool,
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

	created, err := h.svc.Create(systemTenantCtx(h.tenant), h.user, apikeyservice.CreateInput{
		Name:   "integration",
		Scopes: []string{"files:read"},
	})
	require.NoError(t, err)

	resp, body := h.request(t, created.Secret, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var payload struct {
		UserID     uuid.UUID `json:"userID"`
		TenantID   uuid.UUID `json:"tenantID"`
		APIKeyID   uuid.UUID `json:"apiKeyID"`
		AuthMethod string    `json:"authMethod"`
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
	resp, _ = h.request(t, created.Secret, otherSlug)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode, "tenant mismatch")

	// Rotation keeps the old key valid during grace and issues a working new key.
	rotated, err := h.svc.Rotate(systemTenantCtx(h.tenant), h.user, created.ID)
	require.NoError(t, err)
	resp, _ = h.request(t, created.Secret, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "old key within rotation grace")
	resp, _ = h.request(t, rotated.Secret, "")
	assert.Equal(t, fiber.StatusOK, resp.StatusCode, "new key after rotation")

	// Revocation is immediate.
	require.NoError(t, h.svc.Revoke(systemTenantCtx(h.tenant), rotated.ID))
	resp, _ = h.request(t, rotated.Secret, "")
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode, "revoked key")
}

func TestIntegration_APIKeyTenantBinding(t *testing.T) {
	h := setupAPIKeyHarness(t)

	keyB, err := h.svc.Create(systemTenantCtx(h.other), h.userB, apikeyservice.CreateInput{
		Name:   "tenant-b",
		Scopes: []string{"files:read"},
	})
	require.NoError(t, err)

	// No header: the key's tenant is authoritative, so a non-default key works.
	// The hash lookup bypasses RLS (possession proof), while the request's
	// default tenant must not leak into the last-used write.
	resp, body := h.request(t, keyB.Secret, "")
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var payload struct {
		TenantID uuid.UUID `json:"tenantID"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	assert.Equal(t, h.other, payload.TenantID)

	var lastUsed *time.Time
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT last_used_at FROM api_keys WHERE id = $1 AND tenant_id = $2`,
		keyB.ID, h.other,
	).Scan(&lastUsed))
	assert.NotNil(t, lastUsed, "last use must be recorded under the key's own tenant")

	// An explicit default-tenant header conflicts with the key binding.
	resp, _ = h.request(t, keyB.Secret, tenant.DefaultSlug)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

// memberSubjectCtx is a request context for a member of tenantID with the given
// role and permissions, as the authenticate middleware would build it.
func memberSubjectCtx(tenantID, userID uuid.UUID, role string, permissions ...authz.Permission) context.Context {
	return authz.WithSubject(
		tenant.WithID(context.Background(), tenantID),
		authz.NewSubjectWithPermissions(userID, tenantID, role, permissions...),
	)
}

func requireAPIKeyStatus(t *testing.T, err error, status int) {
	t.Helper()
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, status, resolvedStatus(appErr), "error: %v", err)
}

// TestIntegration_APIKeyManagementRespectsMemberRank proves the apikeys to
// identity rank wiring against PostgreSQL: managing another member's key
// requires outranking its owner, and only the owner may widen the scopes.
func TestIntegration_APIKeyManagementRespectsMemberRank(t *testing.T) {
	h := setupAPIKeyHarness(t)
	ctx := context.Background()
	owner := testutil.CreateMember(t, ctx, h.pool, h.tenant, "apikey-rank-owner@test.com", testutil.MemberOptions{Role: "owner"}).UserID
	admin := testutil.CreateMember(t, ctx, h.pool, h.tenant, "apikey-rank-admin@test.com", testutil.MemberOptions{Role: "admin"}).UserID
	member := testutil.CreateMember(t, ctx, h.pool, h.tenant, "apikey-rank-member@test.com", testutil.MemberOptions{Role: "member"}).UserID

	ownerKey, err := h.svc.Create(systemTenantCtx(h.tenant), owner, apikeyservice.CreateInput{
		Name: "owner-key", Scopes: []string{"files:read"},
	})
	require.NoError(t, err)
	memberKey, err := h.svc.Create(systemTenantCtx(h.tenant), member, apikeyservice.CreateInput{
		Name: "member-key", Scopes: []string{"files:read", "files:write"},
	})
	require.NoError(t, err)

	// An admin does not outrank an owner: neither update nor revoke succeeds.
	adminCtx := memberSubjectCtx(h.tenant, admin, authz.RoleAdmin, "files:read", "files:write")
	name := "hijacked"
	_, err = h.svc.Update(adminCtx, ownerKey.ID, apikeyservice.UpdateInput{Name: &name})
	requireAPIKeyStatus(t, err, fiber.StatusForbidden)
	requireAPIKeyStatus(t, h.svc.Revoke(adminCtx, ownerKey.ID), fiber.StatusForbidden)

	// The admin outranks a member: it may narrow that member's key, not widen it.
	_, err = h.svc.Update(adminCtx, memberKey.ID, apikeyservice.UpdateInput{Scopes: []string{"files:read"}})
	require.NoError(t, err)
	_, err = h.svc.Update(adminCtx, memberKey.ID, apikeyservice.UpdateInput{Scopes: []string{"files:read", "files:write"}})
	requireAPIKeyStatus(t, err, fiber.StatusForbidden)
	require.NoError(t, h.svc.Revoke(adminCtx, memberKey.ID))

	// The owner may still widen its own key.
	ownerCtx := memberSubjectCtx(h.tenant, owner, authz.RoleOwner, "files:read", "files:write")
	updated, err := h.svc.Update(ownerCtx, ownerKey.ID, apikeyservice.UpdateInput{Scopes: []string{"files:read", "files:write"}})
	require.NoError(t, err)
	assert.Len(t, updated.Scopes, 2)
}

// apiKeyTestSessions is a permissive session store: this harness authenticates
// with API keys, so the JWT branch only needs a wired store.
type apiKeyTestSessions struct{}

func (apiKeyTestSessions) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{Active: true, TenantActive: true}, nil
}

func (apiKeyTestSessions) Invalidate(context.Context, uuid.UUID) error { return nil }
func (apiKeyTestSessions) InvalidatePermissions(context.Context) error { return nil }
