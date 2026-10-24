package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// countingRefreshService records Consume calls so tests can prove a refusal
// did not burn the refresh token.
type countingRefreshService struct {
	mockRefreshTokenService
	consumed int
}

func (s *countingRefreshService) Consume(ctx context.Context, tokenStr string) (userID, tenantID int64, tokenVersion int32, err error) {
	s.consumed++
	return s.mockRefreshTokenService.Consume(ctx, tokenStr)
}

func TestRefresh_UnverifiedEmailDoesNotConsumeToken(t *testing.T) {
	refreshSvc := &countingRefreshService{}
	h := NewAuthHandler(newMockService(), refreshSvc, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, true, nil)
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Post("/auth/refresh", h.Refresh)

	req, err := http.NewRequest(http.MethodPost, "/auth/refresh",
		bytes.NewBufferString(`{"refreshToken":"valid-refresh-token"}`))
	require.NoError(t, err)
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	require.Zero(t, refreshSvc.consumed, "an unverified-email refusal must not burn the refresh token")
}

type stubGoogleOAuth struct {
	info *oauth.GoogleUserInfo
}

func (s *stubGoogleOAuth) AuthURL(state string) string {
	return "https://accounts.example.test/auth?state=" + url.QueryEscape(state)
}

func (s *stubGoogleOAuth) Exchange(context.Context, string) (*oauth.GoogleUserInfo, error) {
	return s.info, nil
}

func (s *stubGoogleOAuth) BuildCallbackURL(accessToken, refreshToken string) string {
	return "https://frontend.example.test/cb#a=" + accessToken + "&r=" + refreshToken
}

func TestGoogleCallback_RequiresVerifiedEmailWhenConfigured(t *testing.T) {
	provider := &stubGoogleOAuth{info: &oauth.GoogleUserInfo{ID: "g-1", Email: "g@example.com", Name: "G", VerifiedEmail: true}}
	tenants := &fakeTenantRepository{tenant: identitysqlc.Tenant{ID: 1, Slug: "default", Status: "active"}}
	// mockUserService returns an account whose email is not verified, as for
	// a Google-linked account whose address was later changed.
	h := NewAuthHandler(newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, true, provider, tenants)
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Get("/auth/google/callback", h.GoogleCallback)

	state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 1)
	req, err := http.NewRequest(http.MethodGet,
		"/auth/google/callback?code=c&state="+url.QueryEscape(state), http.NoBody)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusForbidden, resp.StatusCode)
}

// TestGoogleCallback_UserRegistrationGatesSignUp proves the callback passes
// the tenant's user_registration flag to the service, which only refuses
// creating new accounts (existing users keep signing in).
func TestGoogleCallback_UserRegistrationGatesSignUp(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		provider := &stubGoogleOAuth{info: &oauth.GoogleUserInfo{ID: "g-1", Email: "g@example.com", Name: "G", VerifiedEmail: true}}
		tenants := &fakeTenantRepository{tenant: identitysqlc.Tenant{ID: 1, Slug: "default", Status: "active"}}
		svc := newMockService()
		h := NewAuthHandler(svc, &mockRefreshTokenService{}, &mockPasswordResetService{},
			&mockEmailVerificationService{}, "test-secret", 24, false, provider, tenants)
		h.SetFeatureFlags(features.NewEvaluator(features.Options{
			Env: map[features.Key]bool{identitycatalog.KeyUserRegistration: enabled},
		}))
		app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
		app.Get("/auth/google/callback", h.GoogleCallback)

		state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 1)
		req, err := http.NewRequest(http.MethodGet,
			"/auth/google/callback?code=c&state="+url.QueryEscape(state), http.NoBody)
		require.NoError(t, err)
		req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
		_, err = app.Test(req)
		require.NoError(t, err)
		require.Equal(t, []bool{enabled}, svc.googleAllowSignUp)
	}
}
