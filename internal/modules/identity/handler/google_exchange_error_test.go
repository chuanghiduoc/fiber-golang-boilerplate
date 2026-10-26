package handler

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// errorGoogleOAuth fails the code exchange with a fixed error.
type errorGoogleOAuth struct{ err error }

func (f *errorGoogleOAuth) AuthURL(state string) string {
	return "https://accounts.example.test/?state=" + state
}

func (f *errorGoogleOAuth) Exchange(context.Context, string) (*oauth.GoogleUserInfo, error) {
	return nil, f.err
}

func (f *errorGoogleOAuth) BuildCallbackURL(accessToken, refreshToken string) string {
	return "https://frontend.example.test/#access_token=" + accessToken + "&refresh_token=" + refreshToken
}

func (f *errorGoogleOAuth) BuildMFACallbackURL(challenge string) string {
	return "https://frontend.example.test/#mfa_challenge=" + challenge
}

// A provider 4xx keeps the existing 400; a 5xx or transport failure is a 502
// upstream-unavailable, never a misleading bad request.
func TestGoogleCallback_ExchangeErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		wantCode string
	}{
		{
			name:   "retrieve error 400",
			err:    &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}},
			status: fiber.StatusBadRequest,
		},
		{
			name:     "retrieve error 503",
			err:      &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}},
			status:   fiber.StatusBadGateway,
			wantCode: string(identitycatalog.CodeAuthOAuthUpstreamUnavailable),
		},
		{
			name:   "userinfo 429",
			err:    &oauth.UpstreamError{Op: "userinfo", StatusCode: http.StatusTooManyRequests},
			status: fiber.StatusBadRequest,
		},
		{
			name:     "transport timeout",
			err:      context.DeadlineExceeded,
			status:   fiber.StatusBadGateway,
			wantCode: string(identitycatalog.CodeAuthOAuthUpstreamUnavailable),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenants := &fakeTenantRepository{
				tenant: service.Tenant{ID: 1, Slug: "default", Name: "Default", Status: "active"},
			}
			authHandler := newTestAuthHandler(
				newMockService(), &mockSessions{}, &mockPasswordResetService{},
				&mockEmailVerificationService{}, "test-secret", 24, false, &errorGoogleOAuth{err: tt.err}, tenants,
			)
			authHandler.SetFeatureFlags(googleOAuthEnabled())
			app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
			app.Get("/auth/google/callback", authHandler.GoogleCallback)

			state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 1)
			req, err := http.NewRequest(http.MethodGet, "/auth/google/callback?state="+state+"&code=secret-code", http.NoBody)
			require.NoError(t, err)
			req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, tt.status, resp.StatusCode)
			if tt.wantCode != "" {
				assert.Contains(t, string(body), tt.wantCode)
			}
			assert.NotContains(t, string(body), "secret-code", "the authorization code must never be echoed")
		})
	}
}

// The error log must carry only the class and upstream status; a provider
// error's message (which may embed the code or a response body) must not leak.
func TestGoogleCallback_ExchangeErrorLogOmitsSecrets(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	provider := &errorGoogleOAuth{err: &oauth2.RetrieveError{
		Response:         &http.Response{StatusCode: http.StatusServiceUnavailable},
		ErrorCode:        "invalid_grant",
		ErrorDescription: "super-secret-code was rejected",
	}}
	tenants := &fakeTenantRepository{
		tenant: service.Tenant{ID: 1, Slug: "default", Name: "Default", Status: "active"},
	}
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, provider, tenants,
	)
	authHandler.SetFeatureFlags(googleOAuthEnabled())
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/auth/google/callback", authHandler.GoogleCallback)

	state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 1)
	req, err := http.NewRequest(http.MethodGet, "/auth/google/callback?state="+state+"&code=super-secret-code", http.NoBody)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	out := logs.String()
	assert.Contains(t, out, "error_type=oauth2.RetrieveError")
	assert.Contains(t, out, "upstream_status=503")
	assert.NotContains(t, out, "super-secret-code")
	assert.NotContains(t, out, "invalid_grant")
}
