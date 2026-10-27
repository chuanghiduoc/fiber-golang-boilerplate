//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

type recordingEmailSender struct {
	mu       sync.Mutex
	messages []email.Message
}

func (s *recordingEmailSender) Send(_ context.Context, msg email.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	return nil
}

func (s *recordingEmailSender) WaitFor(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := len(s.messages)
		s.mu.Unlock()
		if got >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (s *recordingEmailSender) Messages() []email.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]email.Message(nil), s.messages...)
}

type tenantAuthLinksHarness struct {
	app       *fiber.App
	pool      *pgxpool.Pool
	verifSvc  authn.EmailVerificationService
	sender    *recordingEmailSender
	tenantTwo uuid.UUID
	userID    uuid.UUID
	user2ID   uuid.UUID
}

func setupTenantAuthLinksHarness(t *testing.T) *tenantAuthLinksHarness {
	t.Helper()
	ctx := context.Background()

	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	tenantTwo := testutil.CreateTenant(t, ctx, pool, "links-acme")
	userID := testutil.CreateMember(t, ctx, pool, tenantTwo, "links@example.com",
		testutil.MemberOptions{Password: testPassphrase, Unverified: true}).UserID
	user2ID := testutil.CreateMember(t, ctx, pool, tenantTwo, "links2@example.com",
		testutil.MemberOptions{Password: testPassphrase}).UserID

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	sender := &recordingEmailSender{}
	recorder := audit.NewPostgresRecorder(fx.Scoped())
	kit := newIdentityKit(fx, appCache, recorder, "links-integration-secret", sender)

	cfg := &config.Config{
		App:       config.AppConfig{Env: "test", BodyLimit: 1 << 20, RequestTimeout: 10 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 10000, StrictWindow: 60 * time.Second, NormalMax: 10000, NormalWindow: 60 * time.Second, RelaxedMax: 10000, RelaxedWindow: 60 * time.Second},
		JWT:       config.JWTConfig{Secret: "links-integration-secret", AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour},
		// require-header mode: password-reset and verification links must be
		// able to complete through the validated ?tenant= fallback.
		Tenant: config.TenantConfig{DefaultSlug: "default", RequireHeader: true},
	}
	application := app.New(cfg, app.Deps{
		Catalogs:       integrationCatalogs(),
		Cache:          appCache,
		Sessions:       kit.sessions,
		Audit:          recorder,
		TenantResolver: identityadapter.NewTenantResolver(kit.stores.Tenants),
	})
	guard := func(action string) *httpapi.GuardPolicy {
		return &httpapi.GuardPolicy{Action: action, Resource: "auth"}
	}
	httpapi.Register(application.HTTP(), httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/password-resets", OperationID: "requestPasswordReset",
		Tags: []string{"auth"}, Summary: "Request a password reset",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.password_reset_requested")}}, kit.auth.ForgotPassword)
	httpapi.Register(application.HTTP(), httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/password-resets/complete", OperationID: "completePasswordReset",
		Tags: []string{"auth"}, Summary: "Complete a password reset",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.password_reset_completed")}}, kit.auth.ResetPassword)
	httpapi.Register(application.HTTP(), httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/email-verifications/complete", OperationID: "completeEmailVerification",
		Tags: []string{"auth"}, Summary: "Verify an email address",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.email_verified")}}, kit.auth.VerifyEmail)

	return &tenantAuthLinksHarness{
		app:       application.Fiber(),
		pool:      pool,
		verifSvc:  kit.verification,
		sender:    sender,
		tenantTwo: tenantTwo,
		userID:    userID,
		user2ID:   user2ID,
	}
}

func (h *tenantAuthLinksHarness) postJSON(t *testing.T, path string, payload any) (status int, respBody []byte) {
	t.Helper()
	return h.postJSONWithTenant(t, path, payload, "")
}

func (h *tenantAuthLinksHarness) postJSONWithTenant(t *testing.T, path string, payload any, slug string) (status int, respBody []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if slug != "" {
		req.Header.Set(tenant.Header, slug)
	}
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, respBody
}

func extractEmailLink(t *testing.T, msg email.Message) *url.URL {
	t.Helper()
	start := strings.Index(msg.HTML, "http")
	require.NotEqual(t, -1, start, "email does not contain a link: %s", msg.HTML)
	rest := msg.HTML[start:]
	end := strings.Index(rest, `"`)
	require.NotEqual(t, -1, end, "email link is not quoted: %s", msg.HTML)
	parsed, err := url.Parse(rest[:end])
	require.NoError(t, err)
	return parsed
}

func TestTenantAuthLinks_ResetAndVerificationCarryTenant(t *testing.T) {
	h := setupTenantAuthLinksHarness(t)

	// Forgot-password is a normal fetch, so the client sends the header. The
	// generated link must still carry the tenant for browser completion.
	status, body := h.postJSONWithTenant(t, "/api/v1/auth/password-resets",
		identitydto.ForgotPasswordRequest{Email: "links@example.com"}, "links-acme")
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)
	require.True(t, h.sender.WaitFor(1, 2*time.Second))
	resetLink := extractEmailLink(t, h.sender.Messages()[0])
	assert.Equal(t, "links-acme", resetLink.Query().Get("tenant"))
	require.NotEmpty(t, resetLink.Query().Get("token"))

	// Reset completes in the same tenant without X-Tenant-Slug.
	newPassword := strings.Join([]string{"New", "password", "fixture"}, "-") + "-2026"
	status, body = h.postJSON(t, "/api/v1/auth/password-resets/complete?tenant=links-acme",
		identitydto.ResetPasswordRequest{
			Token:    resetLink.Query().Get("token"),
			Password: newPassword,
		})
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)

	// Verification link carries the tenant too.
	require.NoError(t, h.verifSvc.SendVerification(
		tenant.WithIdentity(context.Background(), h.tenantTwo, "links-acme"),
		h.userID, "links@example.com",
	))
	require.True(t, h.sender.WaitFor(2, 2*time.Second))
	verifyLink := extractEmailLink(t, h.sender.Messages()[1])
	assert.Equal(t, "links-acme", verifyLink.Query().Get("tenant"))

	status, body = h.postJSON(t, "/api/v1/auth/email-verifications/complete?tenant=links-acme",
		identitydto.VerifyEmailRequest{Token: verifyLink.Query().Get("token")})
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)

	var verified bool
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, h.userID,
	).Scan(&verified))
	assert.True(t, verified, "verification should have happened")

	// Passwords are global: a reset token belongs to the user, not
	// to a tenant, so it completes from any tenant's link. Use a second
	// account so the per-email reset rate limit does not suppress this email.
	status, _ = h.postJSONWithTenant(t, "/api/v1/auth/password-resets",
		identitydto.ForgotPasswordRequest{Email: "links2@example.com"}, "links-acme")
	require.Equal(t, fiber.StatusNoContent, status)
	require.True(t, h.sender.WaitFor(3, 2*time.Second))
	secondReset := extractEmailLink(t, h.sender.Messages()[2])
	assert.Equal(t, "links-acme", secondReset.Query().Get("tenant"))

	anotherPassword := strings.Join([]string{"Another", "password", "fixture"}, "-") + "-2026"
	status, body = h.postJSON(t, "/api/v1/auth/password-resets/complete?tenant=default",
		identitydto.ResetPasswordRequest{
			Token:    secondReset.Query().Get("token"),
			Password: anotherPassword,
		})
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)

	var hash string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT password_hash FROM user_passwords WHERE user_id = $1`, h.user2ID,
	).Scan(&hash))
	assert.True(t, core.VerifyPasswordHash(hash, anotherPassword),
		"the global password must be the reset one")

	// The catalog still owns the CSRF code the cookie mode uses.
	assert.NotEmpty(t, identitycatalog.CodeAuthCSRFFailed)
}
