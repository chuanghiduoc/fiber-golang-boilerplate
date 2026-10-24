package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// tenantTargetFlags evaluates key as enabled by environment but disabled by
// a tenant target for disabledTenant, proving gates use the request tenant.
func tenantTargetFlags(key features.Key, disabledTenant int64) features.Evaluator {
	return features.NewEvaluator(features.Options{
		Env: map[features.Key]bool{key: true},
		Store: features.StoreFunc(func(_ context.Context) (features.Snapshot, error) {
			return features.Snapshot{
				TenantTargets: map[features.Key]map[int64]bool{key: {disabledTenant: false}},
			}, nil
		}),
	})
}

func gateTestApp(authHandler *AuthHandler, tenantID int64) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), tenantID))
		return c.Next()
	})
	app.Post("/auth/register", authHandler.Register)
	app.Post("/auth/login", authHandler.Login)
	app.Get("/auth/google", authHandler.GoogleRedirect)
	app.Get("/auth/google/callback", authHandler.GoogleCallback)
	return app
}

func assertFeatureDisabled(t *testing.T, resp *http.Response) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
	assert.Contains(t, string(body), string(apperror.CodeFeatureDisabled))
}

func testGoogleOAuth() *oauth.GoogleOAuth {
	return oauth.NewGoogleOAuth(config.OAuthConfig{
		GoogleClientID:     "client-id",
		GoogleClientSecret: "client-secret",
		GoogleRedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		FrontendURL:        "http://localhost:3000",
	})
}

func TestRegisterGatedByUserRegistrationFlag(t *testing.T) {
	authHandler := NewAuthHandler(newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, nil)
	authHandler.SetFeatureFlags(tenantTargetFlags(identitycatalog.KeyUserRegistration, 2))
	body := []byte(`{"email":"new@example.com","password":"Password1!","name":"New User"}`)

	for _, tc := range []struct {
		tenantID int64
		disabled bool
	}{{tenantID: 2, disabled: true}, {tenantID: 1, disabled: false}} {
		req, _ := http.NewRequest(http.MethodPost, "/auth/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := gateTestApp(authHandler, tc.tenantID).Test(req)
		require.NoError(t, err)
		if tc.disabled {
			assertFeatureDisabled(t, resp)
		} else {
			assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
		}
	}
}

func TestLoginRequiresVerificationWhenEmailVerificationFlagOn(t *testing.T) {
	authHandler := NewAuthHandler(newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, nil)
	login := []byte(`{"email":"test@example.com","password":"Password1!"}`)
	doLogin := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(login))
		req.Header.Set("Content-Type", "application/json")
		resp, err := gateTestApp(authHandler, 1).Test(req)
		require.NoError(t, err)
		return resp
	}

	// Compiled default (off): an unverified user can log in.
	assert.Equal(t, fiber.StatusOK, doLogin().StatusCode)

	authHandler.SetFeatureFlags(features.NewEvaluator(features.Options{
		Env: map[features.Key]bool{identitycatalog.KeyEmailVerification: true},
	}))
	resp := doLogin()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), string(identitycatalog.CodeAuthEmailNotVerified))
}

func TestGoogleRedirectGatedByGoogleOAuthFlag(t *testing.T) {
	authHandler := NewAuthHandler(newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, testGoogleOAuth())

	// No evaluator installed: the compiled default (on) applies.
	resp, err := gateTestApp(authHandler, 1).Test(oauthTestRequest(t, http.MethodGet, "/auth/google"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusSeeOther, resp.StatusCode)

	authHandler.SetFeatureFlags(tenantTargetFlags(identitycatalog.KeyGoogleOAuth, 7))
	resp, err = gateTestApp(authHandler, 7).Test(oauthTestRequest(t, http.MethodGet, "/auth/google"))
	require.NoError(t, err)
	assertFeatureDisabled(t, resp)

	resp, err = gateTestApp(authHandler, 1).Test(oauthTestRequest(t, http.MethodGet, "/auth/google"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusSeeOther, resp.StatusCode)
}

func TestGoogleCallbackGatedOnSignedStateTenant(t *testing.T) {
	tenants := &fakeTenantRepository{
		tenant: identitysqlc.Tenant{ID: 2, Slug: "acme", Name: "Acme", Status: "active"},
	}
	authHandler := NewAuthHandler(newMockService(), &mockRefreshTokenService{}, &mockPasswordResetService{},
		&mockEmailVerificationService{}, "test-secret", 24, false, testGoogleOAuth(), tenants)
	authHandler.SetFeatureFlags(tenantTargetFlags(identitycatalog.KeyGoogleOAuth, 2))

	// The request context says tenant 1 (enabled), but the signed state names
	// tenant 2, whose target disables Google OAuth.
	state := encodeOAuthState(oauthStateKey("test-secret"), "nonce", 2)
	req, _ := http.NewRequest(http.MethodGet, "/auth/google/callback?state="+state+"&code=x", http.NoBody)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: state})
	resp, err := gateTestApp(authHandler, 1).Test(req)
	require.NoError(t, err)
	assertFeatureDisabled(t, resp)
}
