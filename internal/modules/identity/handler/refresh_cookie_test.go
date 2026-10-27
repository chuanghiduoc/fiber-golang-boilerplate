package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/sessiontest"
)

type cookieSessions struct {
	SessionIssuer
	refreshed string
	loggedOut string
}

func (s *cookieSessions) Issue(context.Context, uuid.UUID, int32, []string) (core.TokenPair, error) {
	return core.TokenPair{AccessToken: "access-1", RefreshToken: "refresh-1"}, nil
}

func (s *cookieSessions) Refresh(_ context.Context, token string, admit authn.Admission) (*authn.RefreshResult, error) {
	s.refreshed = token
	member := loginMember()
	if admit != nil {
		if err := admit(context.Background(), member); err != nil {
			return nil, err
		}
	}
	return &authn.RefreshResult{
		Member: member,
		Tokens: core.TokenPair{AccessToken: "access-2", RefreshToken: "refresh-2"},
	}, nil
}

func (s *cookieSessions) Logout(_ context.Context, token string) error {
	s.loggedOut = token
	return nil
}

type tenantResolver struct{ id, name string }

func (r tenantResolver) Resolve(context.Context, string) (tenant.Tenant, error) {
	return tenant.Tenant{ID: tenant.DefaultID, Slug: r.name}, nil
}

func loginMember() *core.Member {
	userID := testID(2)
	return &core.Member{
		User: core.User{
			ID: userID, Email: "cookie@example.com", Name: "Cookie",
			EmailVerifiedAt: timePtr(time.Now()),
		},
		Membership: core.Membership{
			TenantID: tenant.DefaultID, UserID: userID,
			Role: core.RoleMember, Status: core.MembershipStatusActive,
		},
	}
}

func newCookieApp(t *testing.T, enabled bool) (*fiber.App, *cookieSessions, *AuthHandler) {
	t.Helper()
	cfg := &config.Config{
		App:       config.AppConfig{Env: "test", BodyLimit: 1 << 20, RequestTimeout: 10 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"https://app.example.com"}},
		RateLimit: config.RateLimitConfig{StrictMax: 100, StrictWindow: 60 * time.Second, NormalMax: 100, NormalWindow: 60 * time.Second, RelaxedMax: 100, RelaxedWindow: 60 * time.Second},
		JWT:       config.JWTConfig{Secret: "cookie-handler-test-secret-0123456789", AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour, RefreshCookie: enabled},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	catalogs := app.NewCatalogs()
	require.NoError(t, catalogs.Errors.Add(apperror.Provider{Owner: identitycatalog.Owner, Specs: identitycatalog.Errors()}))
	application := app.New(cfg, app.Deps{
		Sessions:       sessiontest.AllowAll(),
		TenantResolver: tenantResolver{id: "default", name: "default"},
		Catalogs:       catalogs,
	})
	sessions := &cookieSessions{}
	authHandler := NewAuthHandler(AuthDeps{
		Users:    loginUsers{member: loginMember()},
		Sessions: sessions,
	})
	authHandler.SetRefreshCookie(RefreshCookieConfig{
		Enabled: enabled,
		TTL:     time.Hour,
		Origins: []string{"https://app.example.com"},
	})
	httpapi.Register(application.HTTP(), httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "cookieLogin",
		Tags: []string{"auth"}, Summary: "Login",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, NoStore: true}, authHandler.Login)
	httpapi.Register(application.HTTP(), httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/refresh", OperationID: "cookieRefresh",
		Tags: []string{"auth"}, Summary: "Refresh",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitNormal, NoStore: true}, authHandler.Refresh)
	httpapi.Register(application.HTTP(), httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/logout", OperationID: "cookieLogout",
		Tags: []string{"auth"}, Summary: "Logout",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitNormal, NoStore: true}, authHandler.Logout)
	return application.Fiber(), sessions, authHandler
}

type cookieResult struct {
	status int
	cookie *http.Cookie
	body   []byte
}

func callCookie(t *testing.T, fiberApp *fiber.App, path string, body []byte, headers map[string]string, cookie *http.Cookie) cookieResult {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := fiberApp.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == RefreshCookieName {
			found = c
		}
	}
	return cookieResult{status: resp.StatusCode, cookie: found, body: raw}
}

func TestRefreshCookieMode_LoginSetsHostCookieOnly(t *testing.T) {
	fiberApp, _, _ := newCookieApp(t, true)
	body, _ := json.Marshal(dto.LoginRequest{Email: "cookie@example.com", Password: handlerTestPassword})
	res := callCookie(t, fiberApp, "/api/v1/auth/login", body, nil, nil)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	require.NotNil(t, res.cookie, "login must set __Host-refresh")
	assert.Equal(t, "refresh-1", res.cookie.Value)
	assert.True(t, res.cookie.HttpOnly)
	assert.True(t, res.cookie.Secure)
	assert.Equal(t, http.SameSiteStrictMode, res.cookie.SameSite)
	assert.Equal(t, "/", res.cookie.Path)
	assert.Equal(t, 3600, res.cookie.MaxAge)

	var login dto.LoginResponse
	require.NoError(t, json.Unmarshal(res.body, &login))
	assert.NotEmpty(t, login.AccessToken)
	assert.Empty(t, login.RefreshToken, "the refresh token must not reach JavaScript")
}

func TestRefreshCookieMode_RotatesTheCookie(t *testing.T) {
	fiberApp, sessions, _ := newCookieApp(t, true)
	res := callCookie(t, fiberApp, "/api/v1/auth/refresh", nil, map[string]string{"Sec-Fetch-Site": "same-origin"},
		&http.Cookie{Name: RefreshCookieName, Value: "refresh-1"})
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	require.NotNil(t, res.cookie)
	assert.Equal(t, "refresh-2", res.cookie.Value)
	assert.Equal(t, "refresh-1", sessions.refreshed)
	var login dto.LoginResponse
	require.NoError(t, json.Unmarshal(res.body, &login))
	assert.Empty(t, login.RefreshToken)
}

func TestRefreshCookieMode_CSRFMatrix(t *testing.T) {
	fiberApp, _, _ := newCookieApp(t, true)
	cookie := &http.Cookie{Name: RefreshCookieName, Value: "refresh-1"}
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusOK},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"none", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusForbidden},
		{"trusted origin and header", map[string]string{"Origin": "https://app.example.com", "X-CSRF-Token": "1"}, http.StatusOK},
		{"trusted origin without header", map[string]string{"Origin": "https://app.example.com"}, http.StatusForbidden},
		{"untrusted origin with header", map[string]string{"Origin": "https://evil.example.com", "X-CSRF-Token": "1"}, http.StatusForbidden},
		{"no metadata", nil, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callCookie(t, fiberApp, "/api/v1/auth/refresh", nil, tc.headers, cookie)
			assert.Equal(t, tc.want, res.status, string(res.body))
			if tc.want == http.StatusForbidden {
				assert.Contains(t, string(res.body), string(identitycatalog.CodeAuthCSRFFailed))
			}
		})
	}
}

func TestRefreshCookieMode_LogoutClearsCookie(t *testing.T) {
	fiberApp, sessions, _ := newCookieApp(t, true)
	res := callCookie(t, fiberApp, "/api/v1/auth/logout", nil, map[string]string{"Sec-Fetch-Site": "same-origin"},
		&http.Cookie{Name: RefreshCookieName, Value: "refresh-1"})
	require.Equal(t, http.StatusNoContent, res.status, string(res.body))
	require.NotNil(t, res.cookie, "logout must expire __Host-refresh")
	assert.Equal(t, -1, res.cookie.MaxAge)
	assert.Equal(t, "refresh-1", sessions.loggedOut)
}

func TestRefreshJSONMode_NoCookieAndRefreshInBody(t *testing.T) {
	fiberApp, _, _ := newCookieApp(t, false)
	body, _ := json.Marshal(dto.LoginRequest{Email: "cookie@example.com", Password: handlerTestPassword})
	res := callCookie(t, fiberApp, "/api/v1/auth/login", body, nil, nil)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	assert.Nil(t, res.cookie)
	var login dto.LoginResponse
	require.NoError(t, json.Unmarshal(res.body, &login))
	assert.Equal(t, "refresh-1", login.RefreshToken)

	refreshBody, _ := json.Marshal(dto.RefreshRequest{RefreshToken: "refresh-1"})
	res = callCookie(t, fiberApp, "/api/v1/auth/refresh", refreshBody, nil, nil)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	assert.Nil(t, res.cookie)
}

// A cookie in JSON mode is ignored: the configured mode is single, not a
// per-request choice, so a stale cookie cannot silently authenticate.
func TestRefreshJSONMode_IgnoresCookie(t *testing.T) {
	fiberApp, sessions, _ := newCookieApp(t, false)
	res := callCookie(t, fiberApp, "/api/v1/auth/refresh", nil,
		map[string]string{"Sec-Fetch-Site": "same-origin"},
		&http.Cookie{Name: RefreshCookieName, Value: "refresh-1"})
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Empty(t, sessions.refreshed)
}
