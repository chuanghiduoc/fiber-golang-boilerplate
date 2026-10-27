//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	featuresmodule "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features"
	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/repository"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

const featureSecret = "feature-flags-integration-secret"

type featureHarness struct {
	app     *fiber.App
	pool    *pgxpool.Pool
	flags   featureflags.Evaluator
	tenantA uuid.UUID
	tenantB uuid.UUID
	userA   uuid.UUID
	userB   uuid.UUID
	tokenA  string
	tokenB  string
}

func setupFeatureHarness(t *testing.T) *featureHarness {
	t.Helper()
	ctx := context.Background()

	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	tenantB := testutil.CreateTenant(t, ctx, pool, "flags-acme")

	insertUser := func(tenantID uuid.UUID, email string) uuid.UUID {
		t.Helper()
		return testutil.CreateMember(t, ctx, pool, tenantID, email, testutil.MemberOptions{Role: "admin"}).UserID
	}
	userA := insertUser(tenant.DefaultID, "flags-a@test.com")
	userB := insertUser(tenantB, "flags-b@test.com")
	_, err := pool.Exec(ctx, `
		INSERT INTO platform_role_assignments (user_id, role_id)
		SELECT $1, id FROM roles WHERE tenant_id IS NULL AND scope = 'platform' AND key = 'platform_admin'`,
		userA)
	require.NoError(t, err)

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	stores := identityrepo.NewStores(fx.Scoped())
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), appCache)
	// Platform permissions require a phishing-resistant passkey.
	amr := []string{"pwd", "hwk"}
	tokenA := testutil.SessionTokenAMR(t, ctx, pool,
		testutil.Member{UserID: userA, TenantID: tenant.DefaultID, Email: "flags-a@test.com"}, featureSecret, amr)
	tokenB := testutil.SessionTokenAMR(t, ctx, pool,
		testutil.Member{UserID: userB, TenantID: tenantB, Email: "flags-b@test.com"}, featureSecret, amr)

	flags := featureflags.NewEvaluator(featureflags.Options{
		Store:        featureflags.NewPostgresStore(pool),
		Catalog:      integrationCatalogs().Features,
		CacheTTL:     10 * time.Second,
		MaxStaleness: time.Minute,
	})
	repo := featuresrepo.New(pool)
	recorder := audit.NewPostgresRecorder(fx.Scoped())
	handler := featureshandler.NewHandler(featuressvc.New(repo, flags, recorder, featuresrepo.NewTransactor(fx.Tx(), recorder),
		integrationCatalogs().Features, authz.NewAuthorizer(integrationCatalogs().Permissions)))

	cfg := &config.Config{
		App:       config.AppConfig{Env: "test", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		JWT:       config.JWTConfig{Secret: featureSecret, AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{
		Catalogs:       integrationCatalogs(),
		Cache:          appCache,
		Audit:          recorder,
		TenantResolver: identityadapter.NewTenantResolver(stores.Tenants),
		Flags:          flags,
		Sessions:       sessionStore,
	})
	application.Register(featuresmodule.New(handler))

	return &featureHarness{
		app:     application.Fiber(),
		pool:    pool,
		flags:   flags,
		tenantA: tenant.DefaultID,
		tenantB: tenantB,
		userA:   userA,
		userB:   userB,
		tokenA:  tokenA,
		tokenB:  tokenB,
	}
}

func (h *featureHarness) request(t *testing.T, method, path, bearer, slug string, body []byte) (res *http.Response, payload []byte) {
	return h.requestHeaders(t, method, path, bearer, slug, body, nil)
}

func (h *featureHarness) requestHeaders(t *testing.T, method, path, bearer, slug string, body []byte, extra map[string]string) (res *http.Response, payload []byte) {
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
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-Tenant-Slug", slug)
	for key, value := range extra {
		req.Header.Set(key, value)
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

	resp, body := h.request(t, http.MethodGet, "/api/v1/platform/feature-flags", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var list struct {
		Data []featuresdto.FeatureFlagResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Data, len(integrationCatalogs().Features.Definitions()))

	// Create the process-wide row.
	resp, body = h.request(t, http.MethodPost, "/api/v1/platform/feature-flags", h.tokenA, "default",
		[]byte(`{"key":"user_registration","enabled":true,"killSwitch":false,"description":"enabled by test"}`))
	require.Equal(t, fiber.StatusCreated, resp.StatusCode, "body: %s", body)
	var flag featuresdto.FeatureFlagResponse
	require.NoError(t, json.Unmarshal(body, &flag))
	assert.True(t, flag.Configured)
	assert.Equal(t, "enabled by test", flag.Description)

	// A tenant admin disables the flag for their own tenant.
	resp, body = h.request(t, http.MethodPut, "/api/v1/feature-flags/user_registration/targets/tenant", h.tokenB, "flags-acme",
		[]byte(`{"enabled":false}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var tenantTarget featuresdto.FeatureFlagTargetResponse
	require.NoError(t, json.Unmarshal(body, &tenantTarget))
	assert.Equal(t, h.tenantB, tenantTarget.TenantID)
	assert.Nil(t, tenantTarget.UserID)

	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantA}))
	assert.False(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB}))

	// A user target wins over the tenant target.
	resp, body = h.request(t, http.MethodPut,
		"/api/v1/feature-flags/user_registration/targets/user:"+h.userB.String(), h.tokenB, "flags-acme",
		[]byte(`{"enabled":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var userTarget featuresdto.FeatureFlagTargetResponse
	require.NoError(t, json.Unmarshal(body, &userTarget))
	require.NotNil(t, userTarget.UserID)
	assert.Equal(t, h.userB, *userTarget.UserID)
	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	// Kill switch wins over the user target, through a conditional PATCH.
	resp, body = h.request(t, http.MethodGet, "/api/v1/platform/feature-flags/user_registration", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)
	resp, body = h.requestHeaders(t, http.MethodPatch, "/api/v1/platform/feature-flags/user_registration", h.tokenA, "default",
		[]byte(`{"killSwitch":true}`), map[string]string{"If-Match": etag})
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)

	resp, body = h.requestHeaders(t, http.MethodPatch, "/api/v1/platform/feature-flags/user_registration", h.tokenA, "default",
		[]byte(`{"killSwitch":false}`), map[string]string{"If-Match": etag})
	// The ETag from the earlier read is now stale: the UPDATE itself refuses it.
	require.Equal(t, fiber.StatusPreconditionFailed, resp.StatusCode, "body: %s", body)

	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	result := h.flags.Evaluate(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB})
	assert.False(t, result.Enabled)
	assert.Equal(t, featureflags.SourceKillSwitch, result.Source)

	// Tenant A cannot see tenant B's targets.
	resp, body = h.request(t, http.MethodGet, "/api/v1/feature-flags/user_registration/targets", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var tenantAList struct {
		Data []featuresdto.FeatureFlagTargetResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &tenantAList))
	assert.Empty(t, tenantAList.Data)

	// A tenant B admin cannot change process-wide flag state.
	resp, body = h.request(t, http.MethodPatch, "/api/v1/platform/feature-flags/user_registration", h.tokenB, "flags-acme",
		[]byte(`{"enabled":false}`))
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode, "body: %s", body)

	// The platform admin cannot delete the flag while tenant B still has a
	// target: RESTRICT blocks it with 409 and does not name the tenant.
	resp, body = h.request(t, http.MethodDelete, "/api/v1/platform/feature-flags/user_registration", h.tokenA, "default", nil)
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode, "body: %s", body)
	assert.NotContains(t, string(body), "flags-acme")

	// Tenant B removes both targets, then the flag delete succeeds.
	resp, _ = h.request(t, http.MethodDelete,
		"/api/v1/feature-flags/user_registration/targets/user:"+h.userB.String(), h.tokenB, "flags-acme", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	resp, _ = h.request(t, http.MethodDelete, "/api/v1/feature-flags/user_registration/targets/tenant", h.tokenB, "flags-acme", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	resp, body = h.request(t, http.MethodDelete, "/api/v1/platform/feature-flags/user_registration", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode, "body: %s", body)

	// Unknown keys and non-admin callers are rejected.
	resp, _ = h.request(t, http.MethodGet, "/api/v1/platform/feature-flags/typo", h.tokenA, "default", nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	member := testutil.CreateMember(t, context.Background(), h.pool, h.tenantA, "flags-member@test.com",
		testutil.MemberOptions{Role: "member"})
	userToken := testutil.SessionToken(t, context.Background(), h.pool, member, featureSecret)
	resp, _ = h.request(t, http.MethodGet, "/api/v1/platform/feature-flags", userToken, "default", nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	// Every mutation wrote a required intent row before running.
	rows, err := h.pool.Query(context.Background(),
		`SELECT DISTINCT action FROM audit_events WHERE action LIKE 'admin.feature_flag%'`)
	require.NoError(t, err)
	defer rows.Close()
	actions := map[string]bool{}
	for rows.Next() {
		var action string
		require.NoError(t, rows.Scan(&action))
		actions[action] = true
	}
	require.NoError(t, rows.Err())
	for _, action := range []string{
		"admin.feature_flag.updated", "admin.feature_flag.deleted",
		"admin.feature_flag_target.updated", "admin.feature_flag_target.deleted",
	} {
		assert.True(t, actions[action], "missing audit intent %q", action)
	}
}

func TestIntegration_FeatureFlagTargetWithoutDatabaseDefault(t *testing.T) {
	h := setupFeatureHarness(t)
	ctx := context.Background()

	// Tenant B targets a flag no platform admin has configured. The parent row
	// is created with a NULL database default instead of failing the FK.
	resp, body := h.request(t, http.MethodPut, "/api/v1/feature-flags/email_verification/targets/tenant", h.tokenB, "flags-acme",
		[]byte(`{"enabled":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)

	var enabledIsNull bool
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT enabled IS NULL FROM feature_flags WHERE flag_key = 'email_verification'`,
	).Scan(&enabledIsNull))
	assert.True(t, enabledIsNull, "a target must not force a database default")

	if err := h.flags.Invalidate(ctx); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	resultB := h.flags.Evaluate(ctx, identitycatalog.KeyEmailVerification, featureflags.Subject{TenantID: h.tenantB})
	assert.True(t, resultB.Enabled)
	assert.Equal(t, featureflags.SourceTenantTarget, resultB.Source)
	resultA := h.flags.Evaluate(ctx, identitycatalog.KeyEmailVerification, featureflags.Subject{TenantID: h.tenantA})
	assert.False(t, resultA.Enabled)
	assert.Equal(t, featureflags.SourceCompiledDefault, resultA.Source)

	// The admin view reports the anchor row as not configured.
	resp, body = h.request(t, http.MethodGet, "/api/v1/platform/feature-flags/email_verification", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	var detail featuresdto.FeatureFlagResponse
	require.NoError(t, json.Unmarshal(body, &detail))
	assert.False(t, detail.Configured)

	// A platform admin can set a kill switch while leaving the default unset.
	resp, body = h.request(t, http.MethodPatch, "/api/v1/platform/feature-flags/email_verification", h.tokenA, "default",
		[]byte(`{"killSwitch":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode, "body: %s", body)
	if err := h.flags.Invalidate(ctx); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	resultB = h.flags.Evaluate(ctx, identitycatalog.KeyEmailVerification, featureflags.Subject{TenantID: h.tenantB})
	assert.Equal(t, featureflags.SourceKillSwitch, resultB.Source)
}
