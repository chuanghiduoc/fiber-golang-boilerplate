//go:build integration

package service_test

import (
	"context"
	"net/url"
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

func enrollmentSecret(t *testing.T, enrollment *service.MFAEnrollment) string {
	t.Helper()
	u, err := url.Parse(enrollment.OtpauthURI)
	require.NoError(t, err)
	secret := u.Query().Get("secret")
	require.NotEmpty(t, secret)
	return secret
}

// A password reset removes a pending (unconfirmed) enrollment in the same
// transaction that replaces the password.
func TestPasswordReset_DeletesPendingMFAEnrollment(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "reset-mfa@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})

	_, err := e.mfa.EnrollTOTP(ctx, user.UserID, 0, mfaIntegrationPassword)
	require.NoError(t, err)
	require.Equal(t, int64(1), scalar[int64](t, e.pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NULL`, user.UserID))

	require.NoError(t, e.resets.ForgotPassword(ctx, service.ForgotPasswordInput{Email: user.Email}))
	token := e.mail.tokenFor(t, user.Email)
	require.NoError(t, e.resets.ResetPassword(ctx, service.ResetPasswordInput{Token: token, Password: "NewPassword1!2026"}))

	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NULL`, user.UserID))
}

// A re-enrollment must replace the stored pending secret and reset the
// enrollment window in the database (the ON CONFLICT branch of the upsert).
func TestMFAEnroll_ReplacesPendingSecretInDatabase(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "reenroll@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})

	first, err := e.mfa.EnrollTOTP(ctx, user.UserID, 0, mfaIntegrationPassword)
	require.NoError(t, err)
	firstSecret := enrollmentSecret(t, first)

	// Put back a pending row with a stale secret and an old timestamp (an
	// INSERT is not touched by the update trigger), then enroll again.
	_, err = e.pool.Exec(ctx, `DELETE FROM mfa_factors WHERE user_id = $1`, user.UserID)
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `
		INSERT INTO mfa_factors (user_id, type, secret_ciphertext, updated_at)
		VALUES ($1, 'totp', 'sealed:stale-secret', NOW() - INTERVAL '16 minutes')`, user.UserID)
	require.NoError(t, err)

	second, err := e.mfa.EnrollTOTP(ctx, user.UserID, 0, mfaIntegrationPassword)
	require.NoError(t, err)
	secondSecret := enrollmentSecret(t, second)
	require.NotEqual(t, firstSecret, secondSecret, "re-enrollment must generate a new pending secret")
	assert.NotEqual(t, "stale-secret", secondSecret)
	assert.Equal(t, "sealed:"+secondSecret, scalar[string](t, e.pool,
		`SELECT secret_ciphertext FROM mfa_factors WHERE user_id = $1`, user.UserID),
		"the stored pending secret must be the one from the latest enrollment")
	assert.WithinDuration(t, time.Now(), scalar[time.Time](t, e.pool,
		`SELECT updated_at FROM mfa_factors WHERE user_id = $1`, user.UserID), time.Minute,
		"re-enrollment must reset the enrollment timestamp")

	firstCode, err := totp.Code(firstSecret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, firstCode)
	require.Error(t, err, "the replaced secret must no longer confirm")
	assert.Equal(t, "auth.mfa_invalid_code", appCode(err))

	secondCode, err := totp.Code(secondSecret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, secondCode)
	require.NoError(t, err)
}

// A pending enrollment older than the enrollment window cannot be confirmed.
func TestMFAConfirm_ExpiredEnrollmentInDatabase(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "expired-enrollment@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})

	secret, err := totp.GenerateSecret()
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `
		INSERT INTO mfa_factors (user_id, type, secret_ciphertext, updated_at)
		VALUES ($1, 'totp', $2, NOW() - INTERVAL '16 minutes')`,
		user.UserID, "sealed:"+secret)
	require.NoError(t, err)

	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, code)
	require.Error(t, err)
	assert.Equal(t, "auth.mfa_enrollment_expired", appCode(err))
}

// The platform recovery path removes both factors and recovery codes, ends
// every session in every tenant, bumps the token version, and writes its
// Tier 1 audit row.
func TestMFAResetTOTP_PlatformRecovery(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
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
