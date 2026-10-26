//go:build integration

package service_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
)

// A password reset removes a pending (unconfirmed) enrollment in the same
// transaction that replaces the password.
func TestPasswordReset_DeletesPendingMFAEnrollment(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "reset-mfa@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})

	_, err := e.mfa.EnrollTOTP(ctx, user.UserID, 0, mfaIntegrationPassword)
	require.NoError(t, err)
	require.Equal(t, int64(1), scalar[int64](t, e.pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NULL`, user.UserID))

	require.NoError(t, e.resets.ForgotPassword(ctx, service.ForgotPasswordInput{Email: user.Email}))
	token := e.mail.tokenFor(t, user.Email)
	require.NoError(t, e.resets.ResetPassword(ctx, service.ResetPasswordInput{Token: token, Password: "NewPassword1!"}))

	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NULL`, user.UserID))
}

// The platform recovery path removes both factors and recovery codes, ends
// every session in every tenant, bumps the token version, and writes its
// Tier 1 audit row.
func TestMFAResetTOTP_PlatformRecovery(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "mfa-reset-acme")
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "reset-ok@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})
	// The same user also signs in to a second tenant; its session must end too.
	_, err := e.stores.Memberships.Create(tenant.WithID(ctx, acmeID), user.UserID, service.RoleMember)
	if err != nil {
		// Create refuses an existing membership; this fixture is new, so a
		// failure here is a real setup problem.
		t.Fatalf("create acme membership: %v", err)
	}

	secret := enrollSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, code)
	require.NoError(t, err)

	defaultSession, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)
	acmeSession, err := e.sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	require.NoError(t, e.mfa.ResetTOTP(ctx, user.UserID))

	assert.Zero(t, scalar[int64](t, e.pool, `SELECT count(*) FROM mfa_factors WHERE user_id = $1`, user.UserID))
	assert.Zero(t, scalar[int64](t, e.pool, `SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1`, user.UserID))
	assert.NotNil(t, scalar[*time.Time](t, e.pool, `SELECT revoked_at FROM sessions WHERE id = $1`, defaultSession.SessionID))
	assert.NotNil(t, scalar[*time.Time](t, e.pool, `SELECT revoked_at FROM sessions WHERE id = $1`, acmeSession.SessionID))
	assert.Greater(t, scalar[int32](t, e.pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID), user.TokenVersion)
	assert.Equal(t, int64(1), scalar[int64](t, e.pool,
		`SELECT count(*) FROM audit_events WHERE action = 'admin.user.mfa_reset' AND resource_id = $1`,
		strconv.FormatInt(user.UserID, 10)))
}
