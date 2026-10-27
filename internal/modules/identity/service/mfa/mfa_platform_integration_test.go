//go:build integration

package mfa_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
	"github.com/google/uuid"
)

func enrollmentSecret(t *testing.T, enrollment *core.MFAEnrollment) string {
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
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "reset-mfa@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})

	_, err := e.MFA.EnrollTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase)
	require.NoError(t, err)
	require.Equal(t, int64(1), scalar[int64](t, e.Pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NULL`, user.UserID))

	require.NoError(t, e.Resets.ForgotPassword(ctx, authn.ForgotPasswordInput{Email: user.Email}))
	token := e.Mail.TokenFor(t, user.Email)
	require.NoError(t, e.Resets.ResetPassword(ctx, authn.ResetPasswordInput{Token: token, Password: testNewPassword}))

	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NULL`, user.UserID))
}

// A re-enrollment must replace the stored pending secret and reset the
// enrollment window in the database (the ON CONFLICT branch of the upsert).
func TestMFAEnroll_ReplacesPendingSecretInDatabase(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "reenroll@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})

	first, err := e.MFA.EnrollTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase)
	require.NoError(t, err)
	firstSecret := enrollmentSecret(t, first)

	// Put back a pending row with a stale secret and an old timestamp (an
	// INSERT is not touched by the update trigger), then enroll again.
	_, err = e.Pool.Exec(ctx, `DELETE FROM mfa_factors WHERE user_id = $1`, user.UserID)
	require.NoError(t, err)
	_, err = e.Pool.Exec(ctx, `
		INSERT INTO mfa_factors (user_id, type, secret_ciphertext, updated_at)
		VALUES ($1, 'totp', 'sealed:stale-secret', NOW() - INTERVAL '16 minutes')`, user.UserID)
	require.NoError(t, err)

	second, err := e.MFA.EnrollTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase)
	require.NoError(t, err)
	secondSecret := enrollmentSecret(t, second)
	require.NotEqual(t, firstSecret, secondSecret, "re-enrollment must generate a new pending secret")
	assert.NotEqual(t, "stale-secret", secondSecret)
	assert.Equal(t, "sealed:"+secondSecret, scalar[string](t, e.Pool,
		`SELECT secret_ciphertext FROM mfa_factors WHERE user_id = $1`, user.UserID),
		"the stored pending secret must be the one from the latest enrollment")
	assert.WithinDuration(t, time.Now(), scalar[time.Time](t, e.Pool,
		`SELECT updated_at FROM mfa_factors WHERE user_id = $1`, user.UserID), time.Minute,
		"re-enrollment must reset the enrollment timestamp")

	firstCode, err := totp.Code(firstSecret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase, firstCode)
	require.Error(t, err, "the replaced secret must no longer confirm")
	assert.Equal(t, "auth.mfa_invalid_code", pgfixtures.AppCode(err))

	secondCode, err := totp.Code(secondSecret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase, secondCode)
	require.NoError(t, err)
}

// A pending enrollment older than the enrollment window cannot be confirmed.
func TestMFAConfirm_ExpiredEnrollmentInDatabase(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "expired-enrollment@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})

	secret, err := totp.GenerateSecret()
	require.NoError(t, err)
	_, err = e.Pool.Exec(ctx, `
		INSERT INTO mfa_factors (user_id, type, secret_ciphertext, updated_at)
		VALUES ($1, 'totp', $2, NOW() - INTERVAL '16 minutes')`,
		user.UserID, "sealed:"+secret)
	require.NoError(t, err)

	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase, code)
	require.Error(t, err)
	assert.Equal(t, "auth.mfa_enrollment_expired", pgfixtures.AppCode(err))
}

// The platform recovery path removes both factors and recovery codes, ends
// every session in every tenant, bumps the token version, and writes its
// Tier 1 audit row.
func TestMFAResetTOTP_PlatformRecovery(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "mfa-reset-acme")
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "reset-ok@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})
	// The same user also signs in to a second tenant; its session must end too.
	_, err := e.Stores.Memberships.Create(tenant.WithID(ctx, acmeID), user.UserID, core.RoleMember)
	if err != nil {
		// Create refuses an existing membership; this fixture is new, so a
		// failure here is a real setup problem.
		t.Fatalf("create acme membership: %v", err)
	}

	secret := pgfixtures.EnrollTOTPSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase, code)
	require.NoError(t, err)

	// Confirmation bumped the token version; both post-enable sessions carry
	// the second factor, as a real two-step sign-in does.
	enabledVersion := scalar[int32](t, e.Pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID)
	defaultSession, err := e.Sessions.Issue(ctx, user.UserID, enabledVersion, []string{core.AMRPassword, core.AMROTP})
	require.NoError(t, err)
	acmeSession, err := e.Sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, enabledVersion,
		[]string{core.AMRPassword, core.AMROTP})
	require.NoError(t, err)

	require.NoError(t, e.MFA.ResetTOTP(ctx, user.UserID))

	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM mfa_factors WHERE user_id = $1`, user.UserID))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM mfa_recovery_codes WHERE user_id = $1`, user.UserID))
	assert.NotNil(t, scalar[*time.Time](t, e.Pool, `SELECT revoked_at FROM sessions WHERE id = $1`, defaultSession.SessionID))
	assert.NotNil(t, scalar[*time.Time](t, e.Pool, `SELECT revoked_at FROM sessions WHERE id = $1`, acmeSession.SessionID))
	assert.Greater(t, scalar[int32](t, e.Pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID), user.TokenVersion)
	assert.Equal(t, int64(1), scalar[int64](t, e.Pool,
		`SELECT count(*) FROM audit_events WHERE action = 'admin.user.mfa_reset' AND resource_id = $1`,
		user.UserID.String()))
}
