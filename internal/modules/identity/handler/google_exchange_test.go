package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// exchangeGoogleOAuth is a provider that succeeds immediately and records
// whether the callback passes back the PKCE verifier.
type exchangeGoogleOAuth struct {
	info     *oauth.GoogleUserInfo
	verifier string
}

func (f *exchangeGoogleOAuth) AuthURL(state, verifier string) string {
	f.verifier = verifier
	return "https://accounts.example.test/auth?state=" + url.QueryEscape(state)
}

func (f *exchangeGoogleOAuth) Exchange(_ context.Context, _, verifier string) (*oauth.GoogleUserInfo, error) {
	f.verifier = verifier
	return f.info, nil
}

func (f *exchangeGoogleOAuth) BuildCodeCallbackURL(code string) string {
	return "https://frontend.example.test/auth/callback#code=" + url.QueryEscape(code)
}

func (f *exchangeGoogleOAuth) BuildMFACallbackURL(challenge string) string {
	return "https://frontend.example.test/auth/callback#mfa_challenge=" + url.QueryEscape(challenge)
}

// unitSealer is a reversible test double for the data-encryption codec.
type unitSealer struct{}

func (unitSealer) Seal(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }

func (unitSealer) Open(p []byte) ([]byte, error) {
	if len(p) < len("sealed:") || string(p[:len("sealed:")]) != "sealed:" {
		return nil, context.DeadlineExceeded
	}
	return p[len("sealed:"):], nil
}

type exchangeApp struct {
	app     *fiber.App
	handler *AuthHandler
	google  *exchangeGoogleOAuth
}

func newExchangeApp(t *testing.T) *exchangeApp {
	t.Helper()
	provider := &exchangeGoogleOAuth{info: &oauth.GoogleUserInfo{
		ID: "google-1", Email: "google@example.com", Name: "Google User", VerifiedEmail: true,
	}}
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	h := NewAuthHandler(AuthDeps{
		Users:            newMockService(),
		Sessions:         &mockSessions{},
		Resets:           &mockPasswordResetService{},
		Verification:     &mockEmailVerificationService{},
		OAuthStateSecret: "test-secret",
		GoogleOAuth:      provider,
		Tenants: &fakeTenantRepository{tenant: service.Tenant{
			ID: 1, Slug: "default", Name: "Default", Status: service.TenantStatusActive,
		}},
		Cache:  appCache,
		Sealer: unitSealer{},
	})
	h.SetFeatureFlags(googleOAuthEnabled())

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google", h.GoogleRedirect)
	app.Get("/auth/google/callback", h.GoogleCallback)
	app.Post("/auth/google/exchange", h.GoogleExchange)
	// The exchange test can run the redemption under another tenant.
	app.Post("/auth/google/exchange-as-other", func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), 2))
		return h.GoogleExchange(c)
	})
	return &exchangeApp{app: app, handler: h, google: provider}
}

// startCallback runs the redirect and callback and returns the one-time code.
func (x *exchangeApp) startCallback(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/auth/google", http.NoBody)
	require.NoError(t, err)
	resp, err := x.app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, resp.StatusCode)

	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	state := location.Query().Get("state")
	require.NotEmpty(t, state)
	require.NotEmpty(t, x.google.verifier, "the redirect carries a PKCE verifier")

	var cookie *http.Cookie
	for _, candidate := range resp.Cookies() {
		if candidate.Name == oauthStateCookieName {
			cookie = candidate
			break
		}
	}
	require.NotNil(t, cookie)
	cookieState, verifier, ok := decodeOAuthFlow(oauthStateKey("test-secret"), cookie.Value)
	require.True(t, ok)
	require.Equal(t, state, cookieState)
	require.Equal(t, x.google.verifier, verifier, "the verifier is stored server-side, bound to the state")

	callbackReq, err := http.NewRequest(http.MethodGet,
		"/auth/google/callback?state="+url.QueryEscape(state)+"&code=auth-code", http.NoBody)
	require.NoError(t, err)
	callbackReq.AddCookie(cookie)
	callbackResp, err := x.app.Test(callbackReq)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusSeeOther, callbackResp.StatusCode)
	u, err := url.Parse(callbackResp.Header.Get("Location"))
	require.NoError(t, err)
	values, err := url.ParseQuery(u.Fragment)
	require.NoError(t, err)
	require.Empty(t, values.Get("access_token"), "tokens never reach the redirect")
	code := values.Get("code")
	require.NotEmpty(t, code)
	return code
}

func exchangeRequest(t *testing.T, app *fiber.App, path, code string) (status int, body []byte) {
	t.Helper()
	body, err := json.Marshal(dto.GoogleExchangeRequest{Code: code})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, raw
}

func TestGoogleExchange_SingleUse(t *testing.T) {
	x := newExchangeApp(t)
	code := x.startCallback(t)

	status, body := exchangeRequest(t, x.app, "/auth/google/exchange", code)
	require.Equal(t, fiber.StatusOK, status, string(body))
	var tokens dto.LoginResponse
	require.NoError(t, json.Unmarshal(body, &tokens))
	assert.Equal(t, "mock-access-token", tokens.AccessToken)
	assert.Equal(t, "mock-refresh-token", tokens.RefreshToken)
	assert.Equal(t, "google@example.com", tokens.User.Email)

	status, body = exchangeRequest(t, x.app, "/auth/google/exchange", code)
	assert.Equal(t, fiber.StatusUnauthorized, status, "the code is single-use")
	assert.Contains(t, string(body), string(identitycatalog.CodeAuthOAuthCodeInvalid))
}

func TestGoogleExchange_RejectsAnotherTenantWithoutBurningTheCode(t *testing.T) {
	x := newExchangeApp(t)
	code := x.startCallback(t)

	status, body := exchangeRequest(t, x.app, "/auth/google/exchange-as-other", code)
	assert.Equal(t, fiber.StatusUnauthorized, status)
	assert.Contains(t, string(body), string(identitycatalog.CodeAuthOAuthCodeInvalid))

	status, body = exchangeRequest(t, x.app, "/auth/google/exchange", code)
	assert.Equal(t, fiber.StatusOK, status, "a tenant mismatch does not consume the code")
	assert.Contains(t, string(body), "mock-access-token")
}

func TestGoogleExchange_UnknownCode(t *testing.T) {
	x := newExchangeApp(t)
	status, body := exchangeRequest(t, x.app, "/auth/google/exchange", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	assert.Equal(t, fiber.StatusUnauthorized, status)
	assert.Contains(t, string(body), string(identitycatalog.CodeAuthOAuthCodeInvalid))
}
