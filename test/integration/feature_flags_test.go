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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	featuresmodule "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features"
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/repository"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const featureSecret = "feature-flags-integration-secret"

type featureAudit struct {
	events []audit.Event
}

func (r *featureAudit) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

type featureHarness struct {
	app     *fiber.App
	pool    *pgxpool.Pool
	flags   featureflags.Evaluator
	audit   *featureAudit
	tenantA int64
	tenantB int64
	userA   int64
	userB   int64
	tokenA  string
	tokenB  string
}

func setupFeatureHarness(t *testing.T) *featureHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var tenantB int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('flags-acme', 'Flags Acme') RETURNING id`,
	).Scan(&tenantB))

	insertUser := func(tenantID int64, email string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO users (tenant_id, email, name, role)
			 VALUES ($1, $2, $3, 'admin') RETURNING id`,
			tenantID, email, email,
		).Scan(&id))
		return id
	}
	userA := insertUser(tenant.DefaultID, "flags-a@test.com")
	userB := insertUser(tenantB, "flags-b@test.com")

	tokenA, err := token.GenerateWithVersionAndTenant(userA, tenant.DefaultID, "flags-a@test.com", "admin", 0, featureSecret, 24)
	require.NoError(t, err)
	tokenB, err := token.GenerateWithVersionAndTenant(userB, tenantB, "flags-b@test.com", "admin", 0, featureSecret, 24)
	require.NoError(t, err)

	flags := featureflags.NewEvaluator(featureflags.Options{
		Store:        featureflags.NewPostgresStore(pool),
		CacheTTL:     10 * time.Second,
		MaxStaleness: time.Minute,
	})
	repo := featuresrepo.New(pool)
	handler := featureshandler.NewHandler(featuressvc.New(repo, flags))
	recorder := &featureAudit{}

	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		JWT:       config.JWTConfig{Secret: featureSecret},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{
		Cache:          cache.NewMemoryCache(),
		Audit:          recorder,
		TenantResolver: identityadapter.NewTenantResolver(identityrepo.NewTenantRepository(pool)),
		Flags:          flags,
	})
	application.Register(featuresmodule.New(handler))

	return &featureHarness{
		app:     application.Fiber(),
		pool:    pool,
		flags:   flags,
		audit:   recorder,
		tenantA: tenant.DefaultID,
		tenantB: tenantB,
		userA:   userA,
		userB:   userB,
		tokenA:  tokenA,
		tokenB:  tokenB,
	}
}

func (h *featureHarness) request(t *testing.T, method, path, bearer, slug string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if slug != "" {
		req.Header.Set(tenant.Header, slug)
	}
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, respBody
}

func TestIntegration_FeatureFlagAdminLifecycle(t *testing.T) {
	h := setupFeatureHarness(t)

	// List the catalog.
	resp, body := h.request(t, http.MethodGet, "/api/v1/admin/features/flags", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var list struct {
		Data []featuresdto.FlagResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Data, len(featureflags.Catalog()))
	for _, flag := range list.Data {
		assert.False(t, flag.Configured)
	}

	// Set the database default.
	resp, body = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration", h.tokenA, "default",
		[]byte(`{"enabled":true,"killSwitch":false,"description":"enabled by test"}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var flag featuresdto.FlagResponse
	require.NoError(t, json.Unmarshal(body, &flag))
	assert.True(t, flag.Configured)
	assert.Equal(t, "enabled by test", flag.Description)

	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantA}))
	assert.True(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB}))

	// Tenant B target disables only tenant B.
	resp, body = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration/targets", h.tokenB, "flags-acme",
		[]byte(`{"enabled":false}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var tenantTarget featuresdto.TargetResponse
	require.NoError(t, json.Unmarshal(body, &tenantTarget))
	assert.Equal(t, h.tenantB, tenantTarget.TenantID)
	assert.Nil(t, tenantTarget.UserID)

	// A repeated upsert must update the partial-unique target, not duplicate it.
	resp, body = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration/targets", h.tokenB, "flags-acme",
		[]byte(`{"enabled":false}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var repeatedTarget featuresdto.TargetResponse
	require.NoError(t, json.Unmarshal(body, &repeatedTarget))
	assert.Equal(t, tenantTarget.ID, repeatedTarget.ID)

	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantA}))
	assert.False(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB}))

	// User target wins over the tenant target.
	resp, body = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration/targets", h.tokenB, "flags-acme",
		[]byte(`{"userId":`+jsonInt(h.userB)+`,"enabled":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var userTarget featuresdto.TargetResponse
	require.NoError(t, json.Unmarshal(body, &userTarget))
	require.NotNil(t, userTarget.UserID)
	assert.Equal(t, h.userB, *userTarget.UserID)

	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	// Kill switch wins over the user target.
	resp, _ = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration", h.tokenA, "default",
		[]byte(`{"enabled":true,"killSwitch":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	result := h.flags.Evaluate(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB})
	assert.False(t, result.Enabled)
	assert.Equal(t, featureflags.SourceKillSwitch, result.Source)

	// Tenant A cannot see tenant B's targets.
	resp, body = h.request(t, http.MethodGet, "/api/v1/admin/features/flags/user_registration/targets", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var tenantAList struct {
		Data []featuresdto.TargetResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &tenantAList))
	assert.Empty(t, tenantAList.Data)

	// A user target from another tenant is rejected by the composite FK.
	resp, _ = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration/targets", h.tokenB, "flags-acme",
		[]byte(`{"userId":`+jsonInt(h.userA)+`,"enabled":true}`))
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	// Deleting the user target falls back to the tenant target.
	resp, _ = h.request(t, http.MethodDelete,
		"/api/v1/admin/features/flags/user_registration/targets/"+jsonInt(userTarget.ID), h.tokenB, "flags-acme", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.False(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	// Deleting the flag removes all targets and reverts to compiled/env.
	resp, _ = h.request(t, http.MethodDelete, "/api/v1/admin/features/flags/user_registration", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), featureflags.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	var remainingTargets int
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM feature_flag_targets WHERE flag_key = 'user_registration'`,
	).Scan(&remainingTargets))
	assert.Zero(t, remainingTargets, "deleting a flag must cascade its targets")

	// Unknown keys and non-admin callers are rejected.
	resp, _ = h.request(t, http.MethodGet, "/api/v1/admin/features/flags/typo", h.tokenA, "default", nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	userToken, err := token.GenerateWithVersionAndTenant(h.userA, h.tenantA, "flags-a@test.com", "user", 0, featureSecret, 24)
	require.NoError(t, err)
	resp, _ = h.request(t, http.MethodGet, "/api/v1/admin/features/flags", userToken, "default", nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	// Audit events carry flag-specific metadata.
	require.NotEmpty(t, h.audit.events)
	found := false
	for _, event := range h.audit.events {
		if event.Metadata["feature"] == "user_registration" {
			found = true
			break
		}
	}
	assert.True(t, found, "audit events must include feature metadata")
}

func jsonInt(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
