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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	identitydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
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

type tenantResolver struct {
	repo identityrepo.TenantRepository
}

func (r tenantResolver) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	found, err := r.repo.ResolveBySlug(ctx, slug)
	if err != nil {
		return tenant.Tenant{}, err
	}
	return tenant.Tenant{ID: found.ID, Slug: found.Slug, Name: found.Name}, nil
}

type tenantAuthLinksHarness struct {
	app       *fiber.App
	pool      *pgxpool.Pool
	userRepo  identityrepo.UserRepository
	verifSvc  identityservice.EmailVerificationService
	sender    *recordingEmailSender
	tenantTwo int64
	userID    int64
	user2ID   int64
}

func setupTenantAuthLinksHarness(t *testing.T) *tenantAuthLinksHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var tenantTwo int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('links-acme', 'Links Acme') RETURNING id`,
	).Scan(&tenantTwo))

	hash, err := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	require.NoError(t, err)
	var userID, user2ID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, password_hash, name)
		 VALUES ($1, 'links@example.com', $2, 'Links User') RETURNING id`,
		tenantTwo, string(hash),
	).Scan(&userID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, password_hash, name)
		 VALUES ($1, 'links2@example.com', $2, 'Links User Two') RETURNING id`,
		tenantTwo, string(hash),
	).Scan(&user2ID))

	userRepo := identityrepo.NewUserRepository(pool)
	tenantRepo := identityrepo.NewTenantRepository(pool)
	refreshRepo := identityrepo.NewRefreshTokenRepository(pool)
	resetRepo := identityrepo.NewPasswordResetRepository(pool)
	verifRepo := identityrepo.NewEmailVerificationRepository(pool)
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := database.NewTxManager(pool)
	sender := &recordingEmailSender{}

	userSvc := identityservice.NewUserService(userRepo, refreshRepo, false, appCache, txManager, audit.NewPostgresRecorder(pool), session.NewStore(userRepo, appCache))
	refreshSvc := identityservice.NewRefreshTokenService(refreshRepo, 7)
	resetSvc := identityservice.NewPasswordResetService(
		userRepo, resetRepo, refreshRepo, sender, appCache, "https://frontend.example.test", txManager,
	)
	verifSvc := identityservice.NewEmailVerificationService(
		userRepo, verifRepo, sender, appCache, "https://frontend.example.test", txManager,
	)
	authHandler := identityhandler.NewAuthHandler(
		userSvc, refreshSvc, resetSvc, verifSvc,
		"links-integration-secret", 24, false, nil, tenantRepo,
	)

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	// require-header mode: password-reset and verification links must be able
	// to complete through the validated ?tenant= fallback.
	api := app.Group("/api/v1", tenanthttp.Middleware(tenantResolver{repo: tenantRepo}, ""))
	api.Post("/auth/forgot-password", authHandler.ForgotPassword)
	api.Post("/auth/reset-password", authHandler.ResetPassword)
	api.Post("/auth/verify-email", authHandler.VerifyEmail)

	return &tenantAuthLinksHarness{
		app:       app,
		pool:      pool,
		userRepo:  userRepo,
		verifSvc:  verifSvc,
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
	status, body := h.postJSONWithTenant(t, "/api/v1/auth/forgot-password",
		identitydto.ForgotPasswordRequest{Email: "links@example.com"}, "links-acme")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	require.True(t, h.sender.WaitFor(1, 2*time.Second))
	resetLink := extractEmailLink(t, h.sender.Messages()[0])
	assert.Equal(t, "links-acme", resetLink.Query().Get("tenant"))
	require.NotEmpty(t, resetLink.Query().Get("token"))

	// Reset completes in the same tenant without X-Tenant-Slug.
	status, body = h.postJSON(t, "/api/v1/auth/reset-password?tenant=links-acme",
		identitydto.ResetPasswordRequest{
			Token:    resetLink.Query().Get("token"),
			Password: "NewPassword1!",
		})
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)

	// Verification link carries the tenant too.
	require.NoError(t, h.verifSvc.SendVerification(
		tenant.WithIdentity(context.Background(), h.tenantTwo, "links-acme"),
		h.userID, "links@example.com",
	))
	require.True(t, h.sender.WaitFor(2, 2*time.Second))
	verifyLink := extractEmailLink(t, h.sender.Messages()[1])
	assert.Equal(t, "links-acme", verifyLink.Query().Get("tenant"))

	status, body = h.postJSON(t, "/api/v1/auth/verify-email?tenant=links-acme",
		identitydto.VerifyEmailRequest{Token: verifyLink.Query().Get("token")})
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)

	// Cross-tenant completion is rejected even with the correct opaque token.
	// Use a second account so the per-email reset rate limit does not suppress
	// this email.
	status, _ = h.postJSONWithTenant(t, "/api/v1/auth/forgot-password",
		identitydto.ForgotPasswordRequest{Email: "links2@example.com"}, "links-acme")
	require.Equal(t, fiber.StatusOK, status)
	require.True(t, h.sender.WaitFor(3, 2*time.Second))
	secondReset := extractEmailLink(t, h.sender.Messages()[2])

	status, _ = h.postJSON(t, "/api/v1/auth/reset-password?tenant=default",
		identitydto.ResetPasswordRequest{
			Token:    secondReset.Query().Get("token"),
			Password: "AnotherPassword1!",
		})
	assert.Equal(t, fiber.StatusBadRequest, status, "cross-tenant reset must fail")

	user, err := h.userRepo.GetByID(tenant.WithID(context.Background(), h.tenantTwo), h.userID)
	require.NoError(t, err)
	assert.True(t, user.EmailVerifiedAt.Valid, "verification should have happened")
	var tenantID int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT tenant_id FROM password_reset_tokens WHERE user_id = $1`, h.user2ID,
	).Scan(&tenantID))
	assert.Equal(t, h.tenantTwo, tenantID, "reset tokens stay bound to the owning tenant")
}
