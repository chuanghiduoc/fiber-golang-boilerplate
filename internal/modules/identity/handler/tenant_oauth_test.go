package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// googleOAuthEnabled turns on the default-off google_oauth flag for tests
// that exercise the OAuth flow itself.
func googleOAuthEnabled() features.Evaluator {
	return features.NewEvaluator(features.Options{Env: map[features.Key]bool{identitycatalog.KeyGoogleOAuth: true}})
}

func TestOAuthState_RoundTrip(t *testing.T) {
	state := encodeOAuthState(oauthStateKey("test-secret"), "abc123", 7)
	nonce, tenantID, ok := decodeOAuthState(oauthStateKey("test-secret"), state)
	require.True(t, ok)
	require.Equal(t, "abc123", nonce)
	require.Equal(t, int64(7), tenantID)

	for _, invalid := range []string{"", "nodot", ".7", "abc.", "abc.0", "abc.-1", "abc.notanumber", "abc123.7"} {
		_, _, ok := decodeOAuthState(oauthStateKey("test-secret"), invalid)
		require.False(t, ok, "state %q must be rejected", invalid)
	}

	// Changing the tenant (or using another deployment's key) breaks the MAC,
	// even when the attacker also controls the state cookie.
	tampered := strings.Replace(state, "abc123.7.", "abc123.8.", 1)
	_, _, ok = decodeOAuthState(oauthStateKey("test-secret"), tampered)
	require.False(t, ok, "a state with a modified tenant must be rejected")
	_, _, ok = decodeOAuthState(oauthStateKey("other-secret"), state)
	require.False(t, ok, "a state signed with another key must be rejected")
}

func TestGoogleCallback_RejectsForgedStateEvenWithMatchingCookie(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	tenants := &fakeTenantRepository{tenant: service.Tenant{ID: 2, Slug: "acme", Status: "active"}}
	h := newTestAuthHandler(newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth, tenants)
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google/callback", h.GoogleCallback)

	// The pre-HMAC format: nonce.tenantID, set as both query and cookie.
	forged := "nonce.2"
	req := oauthTestRequest(t, http.MethodGet, "/auth/google/callback?code=c&state="+forged)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: forged})
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestGoogleRedirect_BindsTenantToState(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth,
	)

	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New()
	app.Get("/auth/google", func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), 7))
		return authHandler.GoogleRedirect(c)
	})

	req, _ := http.NewRequest(http.MethodGet, "/auth/google", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)

	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	_, tenantID, ok := decodeOAuthState(oauthStateKey("test-secret"), location.Query().Get("state"))
	require.True(t, ok)
	require.Equal(t, int64(7), tenantID)
}

func TestGoogleRedirect_AcceptsTenantQueryWhenMiddlewareIsAbsent(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	tenants := &fakeTenantRepository{
		tenant: service.Tenant{ID: 2, Slug: "acme", Name: "Acme", Status: "active"},
	}
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth, tenants,
	)
	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google", authHandler.GoogleRedirect)

	resp, err := app.Test(oauthTestRequest(t, http.MethodGet, "/auth/google?tenant=acme"))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	_, tenantID, ok := decodeOAuthState(oauthStateKey("test-secret"), location.Query().Get("state"))
	require.True(t, ok)
	require.Equal(t, int64(2), tenantID)

	// Invalid slugs and unknown tenants are rejected, not defaulted.
	resp, err = app.Test(oauthTestRequest(t, http.MethodGet, "/auth/google?tenant=Bad_Slug"))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	resp, err = app.Test(oauthTestRequest(t, http.MethodGet, "/auth/google?tenant=missing"))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestGoogleRedirect_HeaderResolvedTenantWinsOverQuery(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth,
	)
	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google", func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "tenant_id", 1)
		fiber.Locals[string](c, "tenant_slug", "default")
		c.SetContext(tenant.WithIdentity(c.Context(), 1, "default"))
		return authHandler.GoogleRedirect(c)
	})

	resp, err := app.Test(oauthTestRequest(t, http.MethodGet, "/auth/google?tenant=acme"))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	_, tenantID, ok := decodeOAuthState(oauthStateKey("test-secret"), location.Query().Get("state"))
	require.True(t, ok)
	require.Equal(t, int64(1), tenantID, "header-resolved tenant must take precedence")
}

func oauthTestRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path, http.NoBody)
	require.NoError(t, err)
	return req
}

func TestGoogleCallback_UsesStateTenantAndRejectsUnknown(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	tenants := &fakeTenantRepository{
		tenant: service.Tenant{ID: 2, Slug: "acme", Name: "Acme", Status: "active"},
	}
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth, tenants,
	)
	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google/callback", authHandler.GoogleCallback)

	// The state names a different tenant than the cookie/state's active tenant.
	state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 9)
	req, _ := http.NewRequest(http.MethodGet, "/auth/google/callback?state="+state+"&code=x", http.NoBody)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	// A mismatched state and cookie is rejected before tenant resolution.
	otherState := encodeOAuthState(oauthStateKey("test-secret"), "other", 2)
	req, _ = http.NewRequest(http.MethodGet, "/auth/google/callback?state="+otherState+"&code=x", http.NoBody)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err = app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestGoogleCallback_RejectsSuspendedTenant(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	tenants := &fakeTenantRepository{
		tenant: service.Tenant{ID: 2, Slug: "acme", Name: "Acme", Status: "suspended"},
	}
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth, tenants,
	)
	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google/callback", authHandler.GoogleCallback)

	state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 2)
	req, _ := http.NewRequest(http.MethodGet, "/auth/google/callback?state="+state+"&code=x", http.NoBody)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}
