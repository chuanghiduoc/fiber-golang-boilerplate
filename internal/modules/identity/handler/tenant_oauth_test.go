package handler

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

type fakeTenantRepository struct {
	tenant identitysqlc.Tenant
	err    error
}

var _ identityrepo.TenantRepository = (*fakeTenantRepository)(nil)

func (f *fakeTenantRepository) ResolveBySlug(_ context.Context, slug string) (identitysqlc.Tenant, error) {
	if f.err != nil {
		return identitysqlc.Tenant{}, f.err
	}
	if f.tenant.Slug != slug {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	return f.tenant, nil
}

func (f *fakeTenantRepository) GetByID(_ context.Context, id int64) (identitysqlc.Tenant, error) {
	if f.err != nil {
		return identitysqlc.Tenant{}, f.err
	}
	if f.tenant.ID != id {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	return f.tenant, nil
}

func TestOAuthState_RoundTrip(t *testing.T) {
	state := encodeOAuthState("abc123", 7)
	nonce, tenantID, ok := decodeOAuthState(state)
	require.True(t, ok)
	require.Equal(t, "abc123", nonce)
	require.Equal(t, int64(7), tenantID)

	for _, invalid := range []string{"", "nodot", ".7", "abc.", "abc.0", "abc.-1", "abc.notanumber"} {
		_, _, ok := decodeOAuthState(invalid)
		require.False(t, ok, "state %q must be rejected", invalid)
	}
}

func TestGoogleRedirect_BindsTenantToState(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	authHandler := NewAuthHandler(
		newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth,
	)

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
	_, tenantID, ok := decodeOAuthState(location.Query().Get("state"))
	require.True(t, ok)
	require.Equal(t, int64(7), tenantID)
}

func TestGoogleCallback_UsesStateTenantAndRejectsUnknown(t *testing.T) {
	googleOAuth := oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
	tenants := &fakeTenantRepository{
		tenant: identitysqlc.Tenant{ID: 2, Slug: "acme", Name: "Acme", Status: "active"},
	}
	authHandler := NewAuthHandler(
		newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, googleOAuth, tenants,
	)
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Get("/auth/google/callback", authHandler.GoogleCallback)

	// The state names a different tenant than the cookie/state's active tenant.
	state := encodeOAuthState("nonce", 9)
	req, _ := http.NewRequest(http.MethodGet, "/auth/google/callback?state="+state+"&code=x", http.NoBody)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	// A mismatched state and cookie is rejected before tenant resolution.
	otherState := encodeOAuthState("other", 2)
	req, _ = http.NewRequest(http.MethodGet, "/auth/google/callback?state="+otherState+"&code=x", http.NoBody)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err = app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}
