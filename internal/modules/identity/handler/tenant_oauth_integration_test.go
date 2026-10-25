//go:build integration

package handler

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

type fakeGoogleOAuth struct {
	info *oauth.GoogleUserInfo
}

func (f *fakeGoogleOAuth) AuthURL(state string) string {
	return "https://accounts.example.test/o/oauth2/auth?state=" + url.QueryEscape(state)
}

func (f *fakeGoogleOAuth) Exchange(context.Context, string) (*oauth.GoogleUserInfo, error) {
	return f.info, nil
}

func (f *fakeGoogleOAuth) BuildCallbackURL(accessToken, refreshToken string) string {
	return "https://frontend.example.test/auth/callback#access_token=" + accessToken +
		"&refresh_token=" + refreshToken
}

type tenantOAuthHarness struct {
	pool        *pgxpool.Pool
	userRepo    repository.UserRepository
	activeID    int64
	suspendedID int64
}

func setupOAuthIntegrationApp(t *testing.T, info *oauth.GoogleUserInfo) (*fiber.App, *tenantOAuthHarness) {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var activeID, suspendedID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name, status) VALUES ('oauth-acme', 'OAuth Acme', 'active') RETURNING id`,
	).Scan(&activeID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name, status) VALUES ('oauth-suspended', 'OAuth Suspended', 'suspended') RETURNING id`,
	).Scan(&suspendedID))

	userRepo := repository.NewUserRepository(pool)
	tenantRepo := repository.NewTenantRepository(pool)
	refreshRepo := repository.NewRefreshTokenRepository(pool)
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := database.NewTxManager(pool)
	userSvc := identityservice.NewUserService(userRepo, refreshRepo, false, appCache, txManager, audit.Noop{})
	refreshSvc := identityservice.NewRefreshTokenService(refreshRepo, 7)
	authHandler := NewAuthHandler(
		userSvc, refreshSvc, nil, nil, "oauth-integration-secret", 24, false,
		&fakeGoogleOAuth{info: info}, tenantRepo,
	)

	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	// require-header mode: defaultSlug is empty, so only a header or the
	// validated browser `tenant` query can start a flow.
	api := app.Group("/api/v1", tenanthttp.Middleware(tenantResolver{repo: tenantRepo}, ""))
	api.Get("/auth/google", authHandler.GoogleRedirect)
	api.Get("/auth/google/callback", authHandler.GoogleCallback)

	return app, &tenantOAuthHarness{
		pool:        pool,
		userRepo:    userRepo,
		activeID:    activeID,
		suspendedID: suspendedID,
	}
}

type tenantResolver struct {
	repo repository.TenantRepository
}

func (r tenantResolver) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	found, err := r.repo.ResolveBySlug(ctx, slug)
	if err != nil {
		return tenant.Tenant{}, err
	}
	return tenant.Tenant{ID: found.ID, Slug: found.Slug, Name: found.Name}, nil
}

func TestOAuthIntegration_NonDefaultTenantStartAndCallback(t *testing.T) {
	app, h := setupOAuthIntegrationApp(t, &oauth.GoogleUserInfo{
		ID: "google-user-1", Email: "oauth@example.com", Name: "OAuth User", VerifiedEmail: true,
	})

	// Start: browser navigation can only send ?tenant=; require-header mode
	// would 404 if the middleware did not honor it.
	start, err := app.Test(mustOAuthRequest(t, http.MethodGet, "/api/v1/auth/google?tenant=oauth-acme"))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, start.StatusCode)

	location, err := url.Parse(start.Header.Get("Location"))
	require.NoError(t, err)
	state := location.Query().Get("state")
	_, stateTenantID, ok := decodeOAuthState(oauthStateKey("oauth-integration-secret"), state)
	require.True(t, ok)
	assert.Equal(t, h.activeID, stateTenantID)

	var stateCookie *http.Cookie
	for _, cookie := range start.Cookies() {
		if cookie.Name == oauthStateCookieName {
			stateCookie = cookie
			break
		}
	}
	require.NotNil(t, stateCookie)
	assert.Equal(t, state, stateCookie.Value)

	// Callback: no X-Tenant-Slug, and an explicitly conflicting header must be
	// ignored because the signed state is authoritative.
	callbackPath := "/api/v1/auth/google/callback?state=" + url.QueryEscape(state) + "&code=test-code"
	req := mustOAuthRequest(t, http.MethodGet, callbackPath)
	req.AddCookie(stateCookie)
	req.Header.Set(tenant.Header, "default")
	callback, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, callback.StatusCode)
	assert.Contains(t, callback.Header.Get("Location"), "access_token=")

	// The account and refresh token were created in the state tenant, not the
	// conflicting header tenant or the default tenant.
	user, err := h.userRepo.GetByEmail(tenant.WithID(context.Background(), h.activeID), "oauth@example.com")
	require.NoError(t, err)
	assert.Equal(t, h.activeID, user.TenantID)

	var tokenTenant int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT tenant_id FROM refresh_tokens WHERE user_id = $1`, user.ID,
	).Scan(&tokenTenant))
	assert.Equal(t, h.activeID, tokenTenant)

	_, err = h.userRepo.GetByEmail(tenant.WithID(context.Background(), tenant.DefaultID), "oauth@example.com")
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

func TestOAuthIntegration_CallbackRejectsSuspendedAndUnknownTenant(t *testing.T) {
	app, h := setupOAuthIntegrationApp(t, &oauth.GoogleUserInfo{
		ID: "google-user-2", Email: "suspended@example.com", Name: "Suspended User", VerifiedEmail: true,
	})

	for name, tenantID := range map[string]int64{
		"suspended": h.suspendedID,
		"unknown":   h.activeID + h.suspendedID + 1000,
	} {
		t.Run(name, func(t *testing.T) {
			state := encodeOAuthState(oauthStateKey("oauth-integration-secret"), "nonce-"+name, tenantID)
			req := mustOAuthRequest(t, http.MethodGet,
				"/api/v1/auth/google/callback?state="+url.QueryEscape(state)+"&code=test-code")
			req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
			resp, err := app.Test(req)
			require.NoError(t, err)
			assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
		})
	}

	// Starting a flow for a suspended tenant is rejected during start too.
	resp, err := app.Test(mustOAuthRequest(t, http.MethodGet,
		"/api/v1/auth/google?tenant=oauth-suspended"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// Nothing was created for the failed flows.
	var count int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = 'suspended@example.com'`,
	).Scan(&count))
	assert.Zero(t, count)
}

func mustOAuthRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path, http.NoBody)
	require.NoError(t, err)
	return req
}
