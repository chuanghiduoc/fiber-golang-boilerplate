//go:build integration

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// identityApp mounts the identity handlers like module.go does, behind the
// real tenant middleware (X-Tenant-Slug, default "default") and JWT/session
// validation.
func identityApp(h *identityHarness) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := app.Group("/api/v1", tenanthttp.Middleware(identityadapter.NewTenantResolver(h.stores.Tenants), "default"))
	jwt := middleware.JWTAuth(harnessSecret, h.sessionStore)

	userHandler := NewUserHandler(h.users)
	sessionHandler := NewSessionHandler(h.sessions)
	invitationHandler := NewInvitationHandler(h.invitations)

	auth := api.Group("/auth")
	auth.Post("/register", h.auth.Register)
	auth.Post("/login", h.auth.Login)
	auth.Post("/refresh", h.auth.Refresh)
	auth.Post("/logout", h.auth.Logout)
	auth.Post("/logout-all", jwt, authzhttp.Require(identitycatalog.PermissionSessionsRevoke), h.auth.LogoutAll)
	auth.Get("/sessions", jwt, authzhttp.Require(identitycatalog.PermissionProfileRead), sessionHandler.List)
	auth.Delete("/sessions/:id", jwt, authzhttp.Require(identitycatalog.PermissionSessionsRevoke), sessionHandler.Revoke)
	auth.Post("/invitations/accept", jwt, authzhttp.Require(identitycatalog.PermissionProfileWrite), invitationHandler.Accept)
	auth.Post("/invitations/signup", invitationHandler.SignUp)
	auth.Get("/google", h.auth.GoogleRedirect)
	auth.Get("/google/callback", h.auth.GoogleCallback)

	invitations := api.Group("/invitations", jwt)
	invitations.Post("/", authzhttp.Require(identitycatalog.PermissionInvitationsWrite), invitationHandler.Create)
	invitations.Get("/", authzhttp.Require(identitycatalog.PermissionInvitationsRead), invitationHandler.List)
	invitations.Delete("/:id", authzhttp.Require(identitycatalog.PermissionInvitationsWrite), invitationHandler.Revoke)

	users := api.Group("/users", jwt)
	users.Get("/me", authzhttp.Require(identitycatalog.PermissionProfileRead), userHandler.GetMe)
	users.Delete("/:id", authzhttp.Require(identitycatalog.PermissionProfileWrite), userHandler.Delete)
	users.Get("/", authzhttp.Require(authz.PermissionAdminUsersRead), userHandler.List)
	return app
}

type call struct {
	method, path, tenant, bearer string
	body                         any
}

func do(t *testing.T, app *fiber.App, c call) (status int, body []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if c.body != nil {
		raw, err := json.Marshal(c.body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(c.method, c.path, reader)
	require.NoError(t, err)
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set(fiber.HeaderUserAgent, "integration-test/1.0")
	if c.tenant != "" {
		req.Header.Set(tenant.Header, c.tenant)
	}
	if c.bearer != "" {
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+c.bearer)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 15 * time.Second})
	require.NoError(t, err)
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

func login(t *testing.T, app *fiber.App, slug, email, password string) (int, dto.LoginResponse) {
	t.Helper()
	status, body := do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/login", tenant: slug,
		body: dto.LoginRequest{Email: email, Password: password}})
	var out dto.LoginResponse
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &out))
	}
	return status, out
}

func mustLogin(t *testing.T, app *fiber.App, slug, email, password string) dto.LoginResponse {
	t.Helper()
	status, out := login(t, app, slug, email, password)
	require.Equal(t, http.StatusOK, status)
	return out
}

func me(t *testing.T, app *fiber.App, slug, access string) int {
	t.Helper()
	status, _ := do(t, app, call{method: http.MethodGet, path: "/api/v1/users/me", tenant: slug, bearer: access})
	return status
}

func refresh(t *testing.T, app *fiber.App, slug, refreshToken string) (int, dto.LoginResponse) {
	t.Helper()
	status, body := do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/refresh", tenant: slug,
		body: dto.RefreshRequest{RefreshToken: refreshToken}})
	var out dto.LoginResponse
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &out))
	}
	return status, out
}

func TestIntegration_SessionLifecycle(t *testing.T) {
	h := newIdentityHarness(t, nil)
	app := identityApp(h)
	const pw = "Password1!"

	status, body := do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/register",
		body: dto.RegisterRequest{Email: "Life@Example.com", Password: pw, Name: "Life Cycle"}})
	require.Equal(t, http.StatusCreated, status, string(body))
	var registered dto.UserResponse
	require.NoError(t, json.Unmarshal(body, &registered))
	assert.Equal(t, "life@example.com", registered.Email)
	assert.Equal(t, "active", registered.MembershipStatus)

	first := mustLogin(t, app, "", "life@example.com", pw)
	assert.Equal(t, http.StatusOK, me(t, app, "", first.AccessToken))

	// Rotation, then reuse of the consumed token revokes the whole family.
	status, rotated := refresh(t, app, "", first.RefreshToken)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, http.StatusOK, me(t, app, "", rotated.AccessToken))
	status, _ = refresh(t, app, "", first.RefreshToken)
	assert.Equal(t, http.StatusUnauthorized, status, "reused refresh token")
	assert.Equal(t, http.StatusUnauthorized, me(t, app, "", rotated.AccessToken), "the family's access tokens die with the session")
	status, _ = refresh(t, app, "", rotated.RefreshToken)
	assert.Equal(t, http.StatusUnauthorized, status, "the family's newest refresh token is revoked too")
	var reason string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT revoked_reason FROM sessions WHERE user_id = $1 ORDER BY id LIMIT 1`, registered.ID).Scan(&reason))
	assert.Equal(t, "refresh_reuse", reason)

	// Two live sessions; the list marks the caller's, and one can be revoked.
	current := mustLogin(t, app, "", "life@example.com", pw)
	other := mustLogin(t, app, "", "life@example.com", pw)
	status, body = do(t, app, call{method: http.MethodGet, path: "/api/v1/auth/sessions", bearer: current.AccessToken})
	require.Equal(t, http.StatusOK, status, string(body))
	var page struct {
		Data []dto.SessionResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Data, 2)
	var otherID int64
	for _, s := range page.Data {
		assert.Equal(t, "integration-test/1.0", s.UserAgent)
		if !s.Current {
			otherID = s.ID
		}
	}
	require.NotZero(t, otherID)
	status, _ = do(t, app, call{method: http.MethodDelete, path: "/api/v1/auth/sessions/" + itoa(otherID), bearer: current.AccessToken})
	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, http.StatusUnauthorized, me(t, app, "", other.AccessToken))
	assert.Equal(t, http.StatusOK, me(t, app, "", current.AccessToken))

	// Logout revokes the session by refresh-token possession.
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/logout", body: dto.RefreshRequest{RefreshToken: current.RefreshToken}})
	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, http.StatusUnauthorized, me(t, app, "", current.AccessToken))

	// logout-all ends every session and bumps the token version.
	a := mustLogin(t, app, "", "life@example.com", pw)
	b := mustLogin(t, app, "", "life@example.com", pw)
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/logout-all", bearer: a.AccessToken})
	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, http.StatusUnauthorized, me(t, app, "", a.AccessToken))
	assert.Equal(t, http.StatusUnauthorized, me(t, app, "", b.AccessToken))
	status, _ = refresh(t, app, "", b.RefreshToken)
	assert.Equal(t, http.StatusUnauthorized, status)

	// Self-deletion deletes the global account.
	last := mustLogin(t, app, "", "life@example.com", pw)
	status, _ = do(t, app, call{method: http.MethodDelete, path: "/api/v1/users/" + itoa(registered.ID), bearer: last.AccessToken})
	assert.Equal(t, http.StatusNoContent, status)
	status, _ = login(t, app, "", "life@example.com", pw)
	assert.Equal(t, http.StatusUnauthorized, status)
	var userStatus string
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT status FROM users WHERE id = $1`, registered.ID).Scan(&userStatus))
	assert.Equal(t, "deleted", userStatus)
}

// tokenInLink matches only invitation links: registration also emails a
// verification link to the same address, sent after commit, so it can arrive
// after the invitation and must not be mistaken for it.
var tokenInLink = regexp.MustCompile(`/accept-invitation\?[^"\s]*token=([0-9a-f]{64})`)

// waitForToken returns the token of the newest invitation email sent to addr.
func waitForToken(t *testing.T, mail *recordingSender, addr string) string {
	t.Helper()
	var tok string
	require.Eventually(t, func() bool {
		msgs := mail.messages()
		for i := len(msgs) - 1; i >= 0; i-- {
			if len(msgs[i].To) == 1 && msgs[i].To[0] == addr {
				if m := tokenInLink.FindStringSubmatch(msgs[i].HTML); m != nil {
					tok = m[1]
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "no email with a token for %s", addr)
	return tok
}

func TestIntegration_InvitationJoinsAnotherTenant(t *testing.T) {
	h := newIdentityHarness(t, nil)
	app := identityApp(h)
	ctx := context.Background()
	const pw = "Password1!"

	acme, err := h.tenants.Create(ctx, identityservice.NewTenant{Slug: "acme", Name: "Acme", Status: identityservice.TenantStatusActive})
	require.NoError(t, err)
	testutil.CreateMember(t, ctx, h.pool, acme.ID, "boss@acme.test", testutil.MemberOptions{Role: "admin", Password: pw})
	boss := mustLogin(t, app, "acme", "boss@acme.test", pw)

	status, body := do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/register",
		body: dto.RegisterRequest{Email: "dev@example.com", Password: pw, Name: "Dev"}})
	require.Equal(t, http.StatusCreated, status, string(body))
	// The same address cannot register a second account in acme.
	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/register", tenant: "acme",
		body: dto.RegisterRequest{Email: "dev@example.com", Password: pw, Name: "Dev"}})
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(body), "user.email_taken")
	status, _ = login(t, app, "acme", "dev@example.com", pw)
	assert.Equal(t, http.StatusUnauthorized, status, "not a member of acme yet")

	// A plain member cannot invite.
	dev := mustLogin(t, app, "", "dev@example.com", pw)
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/invitations/", bearer: dev.AccessToken,
		body: dto.InviteRequest{Email: "x@example.com"}})
	assert.Equal(t, http.StatusForbidden, status)

	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/invitations/", tenant: "acme", bearer: boss.AccessToken,
		body: dto.InviteRequest{Email: "Dev@Example.com"}})
	require.Equal(t, http.StatusCreated, status, string(body))
	var inv dto.InvitationResponse
	require.NoError(t, json.Unmarshal(body, &inv))
	assert.Equal(t, "dev@example.com", inv.Email)
	assert.Equal(t, acme.ID, inv.TenantID)
	assert.NotContains(t, string(body), "token", "the token only travels by email")
	invToken := waitForToken(t, h.mail, "dev@example.com")

	// Accept as the signed-in user (token is from the default tenant).
	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/invitations/accept", bearer: dev.AccessToken,
		body: dto.AcceptInvitationRequest{Token: invToken}})
	require.Equal(t, http.StatusOK, status, string(body))
	var joined dto.UserResponse
	require.NoError(t, json.Unmarshal(body, &joined))
	assert.Equal(t, acme.ID, joined.TenantID)
	assert.Equal(t, "user", joined.Role)

	inAcme := mustLogin(t, app, "acme", "dev@example.com", pw)
	assert.Equal(t, acme.ID, inAcme.User.TenantID)
	assert.Equal(t, http.StatusOK, me(t, app, "acme", inAcme.AccessToken))

	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/invitations/accept", bearer: dev.AccessToken,
		body: dto.AcceptInvitationRequest{Token: invToken}})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Contains(t, string(body), "invitation.invalid", "single use")

	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/invitations/", tenant: "acme", bearer: boss.AccessToken,
		body: dto.InviteRequest{Email: "dev@example.com"}})
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, string(body), "membership.exists")

	// Sign-up from an invitation creates a verified account in acme only.
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/invitations/", tenant: "acme", bearer: boss.AccessToken,
		body: dto.InviteRequest{Email: "new@example.com", Role: "admin"}})
	require.Equal(t, http.StatusCreated, status)
	signupToken := waitForToken(t, h.mail, "new@example.com")
	signup := dto.InvitationSignUpRequest{Token: signupToken, Name: "New Person", Password: pw}
	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/invitations/signup", body: signup})
	assert.Equal(t, http.StatusBadRequest, status, "the request tenant must be the invitation's tenant: %s", body)
	status, body = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/invitations/signup", tenant: "acme", body: signup})
	require.Equal(t, http.StatusCreated, status, string(body))
	var created dto.UserResponse
	require.NoError(t, json.Unmarshal(body, &created))
	assert.True(t, created.EmailVerified)
	assert.Equal(t, "admin", created.Role)
	mustLogin(t, app, "acme", "new@example.com", pw)
	status, _ = login(t, app, "", "new@example.com", pw)
	assert.Equal(t, http.StatusUnauthorized, status, "the invitation joined acme only")

	// Pending invitations can be listed and revoked.
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/invitations/", tenant: "acme", bearer: boss.AccessToken,
		body: dto.InviteRequest{Email: "later@example.com"}})
	require.Equal(t, http.StatusCreated, status)
	laterToken := waitForToken(t, h.mail, "later@example.com")
	status, body = do(t, app, call{method: http.MethodGet, path: "/api/v1/invitations/", tenant: "acme", bearer: boss.AccessToken})
	require.Equal(t, http.StatusOK, status)
	var pending struct {
		Data []dto.InvitationResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &pending))
	require.Len(t, pending.Data, 1)
	status, _ = do(t, app, call{method: http.MethodDelete, path: "/api/v1/invitations/" + itoa(pending.Data[0].ID), tenant: "acme", bearer: boss.AccessToken})
	assert.Equal(t, http.StatusNoContent, status)
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/auth/invitations/signup", tenant: "acme",
		body: dto.InvitationSignUpRequest{Token: laterToken, Name: "Later", Password: pw}})
	assert.Equal(t, http.StatusBadRequest, status, "a revoked invitation cannot be used")
}

func TestOAuthIntegration_NonDefaultTenantStartAndCallback(t *testing.T) {
	info := &oauth.GoogleUserInfo{ID: "google-user-1", Email: "oauth@example.com", Name: "OAuth User", VerifiedEmail: true}
	h := newIdentityHarness(t, &fakeGoogleOAuth{info: info})
	h.auth.SetFeatureFlags(googleOAuthEnabled())
	app := identityApp(h)
	ctx := context.Background()
	acme, err := h.tenants.Create(ctx, identityservice.NewTenant{Slug: "oauth-acme", Name: "OAuth Acme", Status: identityservice.TenantStatusActive})
	require.NoError(t, err)

	state, cookie := startOAuth(t, app, "/api/v1/auth/google?tenant=oauth-acme")
	_, stateTenantID, ok := decodeOAuthState(oauthStateKey(harnessSecret), state)
	require.True(t, ok)
	assert.Equal(t, acme.ID, stateTenantID)

	// An explicitly conflicting header is ignored: the signed state wins.
	status, location := finishOAuth(t, app, state, cookie, "default")
	require.Equal(t, http.StatusSeeOther, status)
	assert.Contains(t, location, "access_token=")

	user, err := h.stores.Users.GetByIdentity(ctx, identityservice.ProviderGoogle, "google-user-1")
	require.NoError(t, err)
	assert.True(t, user.EmailVerified())
	_, err = h.stores.Memberships.GetActive(tenant.WithID(ctx, acme.ID), user.ID)
	assert.NoError(t, err, "the membership is in the state tenant")
	_, err = h.stores.Memberships.Get(ctx, user.ID)
	assert.Error(t, err, "no membership in the default tenant")
	var sessionTenant int64
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT tenant_id FROM sessions WHERE user_id = $1`, user.ID).Scan(&sessionTenant))
	assert.Equal(t, acme.ID, sessionTenant)

	// Signing in with Google never joins a tenant implicitly.
	state, cookie = startOAuth(t, app, "/api/v1/auth/google")
	status, _ = finishOAuth(t, app, state, cookie, "")
	assert.Equal(t, http.StatusUnauthorized, status)
}

func TestOAuthIntegration_LinkingRequiresVerifiedEmails(t *testing.T) {
	info := &oauth.GoogleUserInfo{ID: "google-user-2", Email: "local@example.com", Name: "Local", VerifiedEmail: true}
	google := &fakeGoogleOAuth{info: info}
	h := newIdentityHarness(t, google)
	h.auth.SetFeatureFlags(googleOAuthEnabled())
	app := identityApp(h)
	ctx := context.Background()
	local := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "local@example.com", testutil.MemberOptions{Unverified: true})

	state, cookie := startOAuth(t, app, "/api/v1/auth/google")
	status, _ := finishOAuth(t, app, state, cookie, "")
	assert.Equal(t, http.StatusConflict, status, "an unverified local account is never linked")

	_, err := h.pool.Exec(ctx, `UPDATE users SET email_verified_at = NOW() WHERE id = $1`, local.UserID)
	require.NoError(t, err)
	info.VerifiedEmail = false
	state, cookie = startOAuth(t, app, "/api/v1/auth/google")
	status, _ = finishOAuth(t, app, state, cookie, "")
	assert.Equal(t, http.StatusBadRequest, status, "an unverified Google address is never linked")

	info.VerifiedEmail = true
	state, cookie = startOAuth(t, app, "/api/v1/auth/google")
	status, _ = finishOAuth(t, app, state, cookie, "")
	require.Equal(t, http.StatusSeeOther, status)
	linked, err := h.stores.Users.GetByIdentity(ctx, identityservice.ProviderGoogle, "google-user-2")
	require.NoError(t, err)
	assert.Equal(t, local.UserID, linked.ID)
}

func TestOAuthIntegration_CallbackRejectsSuspendedAndUnknownTenant(t *testing.T) {
	info := &oauth.GoogleUserInfo{ID: "google-user-3", Email: "suspended@example.com", Name: "S", VerifiedEmail: true}
	h := newIdentityHarness(t, &fakeGoogleOAuth{info: info})
	h.auth.SetFeatureFlags(googleOAuthEnabled())
	app := identityApp(h)
	ctx := context.Background()
	suspended, err := h.tenants.Create(ctx, identityservice.NewTenant{Slug: "oauth-suspended", Name: "S", Status: identityservice.TenantStatusActive})
	require.NoError(t, err)
	_, err = h.tenants.Transition(ctx, suspended.ID, identityservice.TenantStatusSuspended)
	require.NoError(t, err)

	for name, tenantID := range map[string]int64{"suspended": suspended.ID, "unknown": suspended.ID + 1000} {
		t.Run(name, func(t *testing.T) {
			state := encodeOAuthState(oauthStateKey(harnessSecret), "nonce-"+name, tenantID)
			status, _ := finishOAuth(t, app, state, &http.Cookie{Name: oauthStateCookieName, Value: state}, "")
			assert.Equal(t, http.StatusBadRequest, status)
		})
	}
	status, _ := do(t, app, call{method: http.MethodGet, path: "/api/v1/auth/google?tenant=oauth-suspended"})
	assert.Equal(t, http.StatusNotFound, status)

	var count int64
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = 'suspended@example.com'`).Scan(&count))
	assert.Zero(t, count)
}

func startOAuth(t *testing.T, app *fiber.App, path string) (string, *http.Cookie) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	require.NoError(t, err)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	for _, cookie := range resp.Cookies() {
		if cookie.Name == oauthStateCookieName {
			return location.Query().Get("state"), cookie
		}
	}
	t.Fatal("no OAuth state cookie")
	return "", nil
}

func finishOAuth(t *testing.T, app *fiber.App, state string, cookie *http.Cookie, headerSlug string) (status int, location string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/api/v1/auth/google/callback?state="+url.QueryEscape(state)+"&code=test-code", http.NoBody)
	require.NoError(t, err)
	req.AddCookie(cookie)
	if headerSlug != "" {
		req.Header.Set(tenant.Header, headerSlug)
	}
	resp, err := app.Test(req)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header.Get("Location")
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
