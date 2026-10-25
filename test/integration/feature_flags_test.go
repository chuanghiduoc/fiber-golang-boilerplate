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
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

const featureSecret = "feature-flags-integration-secret"

// featureAudit records domain events and implements the intent/completion
// contracts required by the routes' required-mode audithttp.Guard.
type featureAudit struct {
	events  []audit.Event
	intents []audit.Event
}

func (r *featureAudit) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

func (r *featureAudit) RecordID(_ context.Context, event audit.Event) (int64, error) {
	r.intents = append(r.intents, event)
	return int64(len(r.intents)), nil
}

func (r *featureAudit) Complete(context.Context, int64, audit.Completion) error { return nil }

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

	tenantB := testutil.CreateTenant(t, ctx, pool, "flags-acme")

	// User feature-flag targets reference the (tenant, user) membership.
	insertUser := func(tenantID int64, email string) int64 {
		t.Helper()
		return testutil.CreateMember(t, ctx, pool, tenantID, email, testutil.MemberOptions{Role: "admin"}).UserID
	}
	userA := insertUser(tenant.DefaultID, "flags-a@test.com")
	userB := insertUser(tenantB, "flags-b@test.com")
	// Global flag writes require a platform role assignment.
	_, err = pool.Exec(ctx, `
		INSERT INTO platform_role_assignments (user_id, role_id)
		SELECT $1, id FROM roles WHERE tenant_id IS NULL AND scope = 'platform' AND key = 'platform_admin'`,
		userA)
	require.NoError(t, err)

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	stores := identityrepo.NewStores(pool)
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), appCache)
	tokenA := testutil.SessionToken(t, ctx, pool,
		testutil.Member{UserID: userA, TenantID: tenant.DefaultID, Email: "flags-a@test.com"}, featureSecret)
	tokenB := testutil.SessionToken(t, ctx, pool,
		testutil.Member{UserID: userB, TenantID: tenantB, Email: "flags-b@test.com"}, featureSecret)

	flags := featureflags.NewEvaluator(featureflags.Options{
		Store:        featureflags.NewPostgresStore(pool),
		CacheTTL:     10 * time.Second,
		MaxStaleness: time.Minute,
	})
	repo := featuresrepo.New(pool)
	recorder := &featureAudit{}
	handler := featureshandler.NewHandler(featuressvc.New(repo, flags, recorder))

	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		JWT:       config.JWTConfig{Secret: featureSecret},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{
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
		audit:   recorder,
		tenantA: tenant.DefaultID,
		tenantB: tenantB,
		userA:   userA,
		userB:   userB,
		tokenA:  tokenA,
		tokenB:  tokenB,
	}
}

func (h *featureHarness) request(t *testing.T, method, path, bearer, slug string, body []byte) (res *http.Response, payload []byte) {
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
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantA}))
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
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
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantA}))
	assert.False(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
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
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	// Kill switch wins over the user target.
	resp, _ = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration", h.tokenA, "default",
		[]byte(`{"enabled":true,"killSwitch":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	result := h.flags.Evaluate(context.Background(), identitycatalog.KeyUserRegistration,
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
	assert.False(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	// A tenant B admin cannot change or delete process-wide flag state.
	resp, body = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/user_registration", h.tokenB, "flags-acme",
		[]byte(`{"enabled":true,"killSwitch":false}`))
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "permission.denied")
	resp, _ = h.request(t, http.MethodDelete, "/api/v1/admin/features/flags/user_registration", h.tokenB, "flags-acme", nil)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)

	// The platform admin cannot delete the flag while tenant B still has a
	// target: RESTRICT blocks it with 409 and does not name the tenant.
	resp, body = h.request(t, http.MethodDelete, "/api/v1/admin/features/flags/user_registration", h.tokenA, "default", nil)
	assert.Equal(t, fiber.StatusConflict, resp.StatusCode)
	assert.NotContains(t, string(body), "flags-acme")
	var remainingTargets int
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM feature_flag_targets WHERE flag_key = 'user_registration'`,
	).Scan(&remainingTargets))
	assert.Equal(t, 1, remainingTargets, "a blocked flag delete must keep other tenants' targets")

	// Once tenant B removes its own target, the flag delete succeeds and the
	// flag reverts to compiled/env defaults.
	resp, _ = h.request(t, http.MethodDelete,
		"/api/v1/admin/features/flags/user_registration/targets/"+jsonInt(tenantTarget.ID), h.tokenB, "flags-acme", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	resp, _ = h.request(t, http.MethodDelete, "/api/v1/admin/features/flags/user_registration", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	if err := h.flags.Invalidate(context.Background()); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	assert.True(t, h.flags.Enabled(context.Background(), identitycatalog.KeyUserRegistration,
		featureflags.Subject{TenantID: h.tenantB, UserID: h.userB}))

	// Unknown keys and non-admin callers are rejected.
	resp, _ = h.request(t, http.MethodGet, "/api/v1/admin/features/flags/typo", h.tokenA, "default", nil)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	member := testutil.CreateMember(t, context.Background(), h.pool, h.tenantA, "flags-member@test.com",
		testutil.MemberOptions{Role: "member"})
	userToken := testutil.SessionToken(t, context.Background(), h.pool, member, featureSecret)
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

	// Every mutation wrote a required intent row before running.
	actions := make(map[string]bool, len(h.audit.intents))
	for _, intent := range h.audit.intents {
		actions[intent.Action] = true
	}
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
	resp, _ := h.request(t, http.MethodPut, "/api/v1/admin/features/flags/email_verification/targets", h.tokenB, "flags-acme",
		[]byte(`{"enabled":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

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
	assert.Equal(t, featureflags.SourceCompiledDefault, resultA.Source, "other tenants keep env/compiled defaults")

	// The admin view reports the anchor row as not configured.
	resp, body := h.request(t, http.MethodGet, "/api/v1/admin/features/flags/email_verification", h.tokenA, "default", nil)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	var detail featuresdto.FlagDetailResponse
	require.NoError(t, json.Unmarshal(body, &detail))
	assert.False(t, detail.Flag.Configured)

	// A platform admin can set a kill switch while leaving the default unset.
	resp, _ = h.request(t, http.MethodPut, "/api/v1/admin/features/flags/email_verification", h.tokenA, "default",
		[]byte(`{"killSwitch":true}`))
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	if err := h.flags.Invalidate(ctx); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	resultB = h.flags.Evaluate(ctx, identitycatalog.KeyEmailVerification, featureflags.Subject{TenantID: h.tenantB})
	assert.Equal(t, featureflags.SourceKillSwitch, resultB.Source)
}

func jsonInt(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
