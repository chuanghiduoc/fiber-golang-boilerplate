//go:build integration

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

type tenantServiceHarness struct {
	pool        *pgxpool.Pool
	userRepo    repository.UserRepository
	refreshRepo repository.RefreshTokenRepository
	resetRepo   repository.PasswordResetRepository
	verifRepo   repository.EmailVerificationRepository
	refreshSvc  RefreshTokenService
	resetSvc    PasswordResetService
	verifSvc    EmailVerificationService
	sender      *mockEmailSender
	tenantTwo   int64
}

func setupTenantServiceHarness(t *testing.T) *tenantServiceHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var tenantTwo int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('acme', 'Acme') RETURNING id`,
	).Scan(&tenantTwo))

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	txManager := database.NewTxManager(pool)
	sender := newMockEmailSender()

	userRepo := repository.NewUserRepository(pool)
	refreshRepo := repository.NewRefreshTokenRepository(pool)
	resetRepo := repository.NewPasswordResetRepository(pool)
	verifRepo := repository.NewEmailVerificationRepository(pool)

	return &tenantServiceHarness{
		pool:        pool,
		userRepo:    userRepo,
		refreshRepo: refreshRepo,
		resetRepo:   resetRepo,
		verifRepo:   verifRepo,
		refreshSvc:  NewRefreshTokenService(refreshRepo, 7),
		resetSvc: NewPasswordResetService(
			userRepo, resetRepo, refreshRepo, sender, appCache,
			"http://localhost:3000", txManager,
		),
		verifSvc: NewEmailVerificationService(
			userRepo, verifRepo, sender, appCache, "http://localhost:3000", txManager,
		),
		sender:    sender,
		tenantTwo: tenantTwo,
	}
}

func tenantContext(id int64) context.Context {
	return tenant.WithID(context.Background(), id)
}

func extractEmailToken(t *testing.T, msg email.Message) string {
	t.Helper()
	idx := strings.Index(msg.HTML, "token=")
	require.NotEqual(t, -1, idx, "email does not contain a token: %s", msg.HTML)
	rest := msg.HTML[idx+len("token="):]
	if end := strings.IndexAny(rest, `"&<`); end >= 0 {
		rest = rest[:end]
	}
	require.NotEmpty(t, rest)
	return rest
}

func TestTenantIntegration_TokenRowsCarryTenant(t *testing.T) {
	h := setupTenantServiceHarness(t)
	ctxA := tenantContext(tenant.DefaultID)
	ctxB := tenantContext(h.tenantTwo)

	userA, err := h.userRepo.Create(ctxA, identitysqlc.CreateUserParams{
		Email: "a@example.com", Name: "Tenant A",
	})
	require.NoError(t, err)
	require.Equal(t, tenant.DefaultID, userA.TenantID)

	// Password reset tokens: created in A, unusable in B.
	require.NoError(t, h.resetSvc.ForgotPassword(ctxA, dto.ForgotPasswordRequest{Email: userA.Email}))
	require.True(t, h.sender.WaitFor(1, 2*time.Second))
	resetToken := extractEmailToken(t, h.sender.Messages()[0])
	require.Error(t, h.resetSvc.ResetPassword(ctxB, dto.ResetPasswordRequest{
		Token: resetToken, Password: "NewPassword1!",
	}))
	var resetTenant int64
	require.NoError(t, h.pool.QueryRow(ctxA,
		`SELECT tenant_id FROM password_reset_tokens WHERE user_id = $1`, userA.ID,
	).Scan(&resetTenant))
	require.Equal(t, tenant.DefaultID, resetTenant, "reset token must carry the user tenant")
	// The cross-tenant failure must not consume the token.
	require.NoError(t, h.resetSvc.ResetPassword(ctxA, dto.ResetPasswordRequest{
		Token: resetToken, Password: "NewPassword1!",
	}))

	// Email verification tokens: created in A, unusable in B.
	require.NoError(t, h.verifSvc.SendVerification(ctxA, userA.ID, userA.Email))
	require.True(t, h.sender.WaitFor(2, 2*time.Second))
	verifyToken := extractEmailToken(t, h.sender.Messages()[1])
	var verifyTenant int64
	require.NoError(t, h.pool.QueryRow(ctxA,
		`SELECT tenant_id FROM email_verification_tokens WHERE user_id = $1`, userA.ID,
	).Scan(&verifyTenant))
	require.Equal(t, tenant.DefaultID, verifyTenant, "verification token must carry the user tenant")
	require.Error(t, h.verifSvc.Verify(ctxB, verifyToken))
	require.NoError(t, h.verifSvc.Verify(ctxA, verifyToken))

	// Refresh tokens: created in A, unusable in B.
	current, err := h.userRepo.GetByID(ctxA, userA.ID)
	require.NoError(t, err)
	plain, err := h.refreshSvc.Create(ctxA, current.ID, current.TokenVersion)
	require.NoError(t, err)
	var refreshTenant int64
	require.NoError(t, h.pool.QueryRow(ctxA,
		`SELECT tenant_id FROM refresh_tokens WHERE token = $1`, hashToken(plain),
	).Scan(&refreshTenant))
	require.Equal(t, tenant.DefaultID, refreshTenant, "refresh token must carry the user tenant")
	_, _, _, err = h.refreshSvc.Consume(ctxB, plain)
	require.Error(t, err)
	userID, tokenTenant, _, err := h.refreshSvc.Consume(ctxA, plain)
	require.NoError(t, err)
	require.Equal(t, userA.ID, userID)
	require.Equal(t, tenant.DefaultID, tokenTenant)
}
