//go:build integration

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// An expired reset token must actually be removed: the cleanup used to run in
// a transaction that was then rolled back.
func TestReviewIntegration_ExpiredResetTokenIsDeleted(t *testing.T) {
	h := setupTenantServiceHarness(t)
	ctx := tenantContext(tenant.DefaultID)

	user, err := h.userRepo.Create(ctx, identitysqlc.CreateUserParams{
		Email: "expired-reset@example.com", Name: "Expired",
	})
	require.NoError(t, err)
	require.NoError(t, h.resetSvc.ForgotPassword(ctx, dto.ForgotPasswordRequest{Email: user.Email}))
	require.True(t, h.sender.WaitFor(1, 2*time.Second))
	resetToken := extractEmailToken(t, h.sender.Messages()[0])

	_, err = h.pool.Exec(ctx,
		`UPDATE password_reset_tokens SET expires_at = NOW() - INTERVAL '1 minute' WHERE user_id = $1`, user.ID)
	require.NoError(t, err)

	err = h.resetSvc.ResetPassword(ctx, dto.ResetPasswordRequest{Token: resetToken, Password: "NewPassword1!"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")

	var count int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`, user.ID,
	).Scan(&count))
	require.Zero(t, count, "expired reset token must be deleted")
}

// The reset job looks the account up by address outside the token
// transaction. An email change committed in between must not yield a valid
// token (which would be mailed to the old address).
func TestReviewIntegration_ResetTokenRequiresCurrentAddress(t *testing.T) {
	h := setupTenantServiceHarness(t)
	ctx := tenantContext(tenant.DefaultID)

	stale, err := h.userRepo.Create(ctx, identitysqlc.CreateUserParams{
		Email: "Old-Address@example.com", Name: "Mover",
	})
	require.NoError(t, err)

	issued, err := replaceResetToken(ctx, h.resetRepo, stale, "token-for-current-address")
	require.NoError(t, err)
	require.True(t, issued, "the address check is case-insensitive like GetUserByEmail")

	_, err = h.userRepo.Update(ctx, identitysqlc.UpdateUserParams{
		ID: stale.ID, Name: stale.Name, Email: "new-address@example.com", UpdateEmail: true,
	})
	require.NoError(t, err)

	issued, err = replaceResetToken(ctx, h.resetRepo, stale, "token-for-old-address")
	require.NoError(t, err)
	require.False(t, issued, "a token must not be issued for an address the account no longer has")

	var count int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`, stale.ID,
	).Scan(&count))
	require.Zero(t, count)
}

// The scheduled purge removes expired tokens from every tenant and keeps live
// ones.
func TestReviewIntegration_ExpiredTokensArePurged(t *testing.T) {
	h := setupTenantServiceHarness(t)
	ctx := context.Background()

	tenants := []int64{tenant.DefaultID, h.tenantTwo}
	userIDs := make([]int64, 0, len(tenants))
	for _, tenantID := range tenants {
		user, err := h.userRepo.Create(tenantContext(tenantID), identitysqlc.CreateUserParams{
			Email: fmt.Sprintf("purge-%d@example.com", tenantID), Name: "Purge",
		})
		require.NoError(t, err)
		userIDs = append(userIDs, user.ID)
		for i, expiresIn := range []string{"-1 minute", "1 hour"} {
			_, err = h.pool.Exec(ctx, `INSERT INTO refresh_tokens (tenant_id, user_id, token, expires_at)
				VALUES ($1, $2, $3, NOW() + $4::interval)`,
				tenantID, user.ID, fmt.Sprintf("refresh-%d-%d", user.ID, i), expiresIn)
			require.NoError(t, err)
		}
		_, err = h.pool.Exec(ctx, `INSERT INTO password_reset_tokens (tenant_id, user_id, token, expires_at)
			VALUES ($1, $2, $3, NOW() - INTERVAL '1 minute')`, tenantID, user.ID, fmt.Sprintf("reset-%d", user.ID))
		require.NoError(t, err)
		_, err = h.pool.Exec(ctx, `INSERT INTO email_verification_tokens (tenant_id, user_id, email, token, expires_at)
			VALUES ($1, $2, $3, $4, NOW() - INTERVAL '1 minute')`, tenantID, user.ID, user.Email, fmt.Sprintf("verify-%d", user.ID))
		require.NoError(t, err)
	}

	require.NoError(t, NewTokenPurger(repository.NewTokenPurgeRepository(h.pool)).HandleJob(ctx, nil))

	count := func(query string) int64 {
		var n int64
		require.NoError(t, h.pool.QueryRow(ctx, query, userIDs).Scan(&n))
		return n
	}
	require.Equal(t, int64(2), count(`SELECT count(*) FROM refresh_tokens WHERE user_id = ANY($1) AND expires_at > NOW()`))
	require.Zero(t, count(`SELECT count(*) FROM refresh_tokens WHERE user_id = ANY($1) AND expires_at <= NOW()`))
	require.Zero(t, count(`SELECT count(*) FROM password_reset_tokens WHERE user_id = ANY($1)`))
	require.Zero(t, count(`SELECT count(*) FROM email_verification_tokens WHERE user_id = ANY($1)`))
}

// A link built without a request slug (credential tenant overrode a
// header-less default) must carry the user's own tenant slug.
func TestReviewIntegration_VerificationLinkUsesTokenTenantSlug(t *testing.T) {
	h := setupTenantServiceHarness(t)
	ctxB := tenantContext(h.tenantTwo) // tenant ID only, no slug

	userB, err := h.userRepo.Create(ctxB, identitysqlc.CreateUserParams{
		Email: "link-b@example.com", Name: "Link B",
	})
	require.NoError(t, err)

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	svc := NewEmailVerificationService(
		h.userRepo, h.verifRepo, h.sender, appCache, "http://localhost:3000",
		database.NewTxManager(h.pool), repository.NewTenantRepository(h.pool),
	)
	require.NoError(t, svc.SendVerification(ctxB, userB.ID, userB.Email))
	require.True(t, h.sender.WaitFor(1, 2*time.Second))
	html := h.sender.Messages()[0].HTML
	require.True(t, strings.Contains(html, "tenant=acme"), "link must carry the user tenant slug: %s", html)
}

// Concurrent demotions of a tenant's only two admins must leave exactly one
// admin and return a clean 409 to the loser, never a 500 deadlock.
func TestReviewIntegration_ConcurrentAdminDemotionsAreSerialized(t *testing.T) {
	h := setupTenantServiceHarness(t)
	svc := NewAdminUserService(h.userRepo, h.refreshRepo, database.NewTxManager(h.pool), audit.Noop{})

	for round := range 5 {
		// A fresh tenant per round so each round starts with exactly two admins.
		var tenantID int64
		require.NoError(t, h.pool.QueryRow(tenantContext(tenant.DefaultID),
			`INSERT INTO tenants (slug, name) VALUES ($1, 'Race') RETURNING id`, fmt.Sprintf("race-%d", round),
		).Scan(&tenantID))
		ctx := tenantContext(tenantID)

		ids := make([]int64, 2)
		for i := range ids {
			user, err := h.userRepo.Create(ctx, identitysqlc.CreateUserParams{
				Email: fmt.Sprintf("admin-%d@example.com", i), Name: "Admin",
			})
			require.NoError(t, err)
			_, err = h.pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, user.ID)
			require.NoError(t, err)
			ids[i] = user.ID
		}

		var wg sync.WaitGroup
		errs := make([]error, len(ids))
		start := make(chan struct{})
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id int64) {
				defer wg.Done()
				<-start
				_, errs[i] = svc.UpdateRole(ctx, id, dto.RoleUser)
			}(i, id)
		}
		close(start)
		wg.Wait()

		succeeded, conflicted := 0, 0
		for _, err := range errs {
			if err == nil {
				succeeded++
				continue
			}
			var appErr *apperror.AppError
			require.True(t, errors.As(err, &appErr), "round %d: unexpected error %v", round, err)
			require.Equal(t, http.StatusConflict, appErr.Status, "round %d: %v", round, err)
			conflicted++
		}
		require.Equal(t, 1, succeeded, "round %d", round)
		require.Equal(t, 1, conflicted, "round %d", round)

		var admins int64
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE tenant_id = $1 AND role = 'admin' AND deleted_at IS NULL`, tenantID,
		).Scan(&admins))
		require.Equal(t, int64(1), admins, "round %d", round)
	}
}
