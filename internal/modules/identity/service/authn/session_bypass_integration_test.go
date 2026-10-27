//go:build integration

package authn_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
	"github.com/google/uuid"
)

// sessionValidator is the real access-token validator over the application
// stores (no cache, so every call reads the database).
func sessionValidator(e *pgfixtures.Env) session.Store {
	return session.NewStore(identityadapter.NewSessionStateReader(e.Stores.Sessions), nil)
}

// validateForUser checks one session as an HTTP request would: the claims carry
// the token version the client authenticated at.
func validateForUser(t *testing.T, store session.Store, tenantID, userID, sessionID uuid.UUID, tokenVersion int32) error {
	t.Helper()
	_, err := store.Validate(tenant.WithID(context.Background(), tenantID), session.Claims{
		UserID:       userID,
		TenantID:     tenantID,
		SessionID:    sessionID,
		TokenVersion: tokenVersion,
	})
	return err
}

// A password change is a tenant-scoped business transaction plus a
// token-version bump; every session the user holds in any tenant is rejected
// immediately, and the after-commit bypass statement records the revocation.
func TestPasswordChange_RejectsOtherTenantSessionImmediately(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "pw-change-acme")
	password := strings.Join([]string{"correct", "horse", "battery", "staple"}, "-")
	user := testutil.CreateMember(t, ctx, e.Pool, acmeID, "pw-cross@example.com",
		testutil.MemberOptions{Password: password})
	testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "pw-cross@example.com", testutil.MemberOptions{})

	other, err := e.Sessions.Issue(tenant.WithID(ctx, tenant.DefaultID), user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	newPassword := strings.Join([]string{"another", "horse", "battery", "staple"}, "-")
	require.NoError(t, e.Users.ChangePassword(tenant.WithID(ctx, acmeID), user.UserID, authn.ChangePasswordInput{
		CurrentPassword: password,
		NewPassword:     newPassword,
	}))

	// The other tenant's access token is rejected right after the change.
	err = validateForUser(t, sessionValidator(e), tenant.DefaultID, user.UserID, other.SessionID, user.TokenVersion)
	require.Error(t, err)
	assert.Equal(t, 401, pgfixtures.AppStatus(err))

	// Its refresh token cannot mint a new one either.
	_, err = e.Sessions.Refresh(tenant.WithID(ctx, tenant.DefaultID), other.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, 401, pgfixtures.AppStatus(err))

	// The follow-up bypass statement recorded the revocation and its reason.
	assert.Equal(t, string(core.RevokeCredentialsChanged), scalar[string](t, e.Pool,
		`SELECT revoked_reason FROM sessions WHERE id = $1`, other.SessionID))
	assert.NotNil(t, scalar[*time.Time](t, e.Pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, other.SessionID))
}

// The cleanup the credential/MFA changes run after commit only touches rows
// whose token version no longer matches: running it twice is idempotent.
func TestRevokeStaleUserSessions_CleanupIsIdempotent(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "cleanup-idempotent@example.com", testutil.MemberOptions{})

	first, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)
	second, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	// A credential change bumps the version; the sessions are stale.
	_, err = e.Pool.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id = $1`, user.UserID)
	require.NoError(t, err)

	require.NoError(t, e.Stores.Sessions.RevokeStaleForUser(ctx, user.UserID, core.RevokeCredentialsChanged))
	require.NoError(t, e.Stores.Sessions.RevokeStaleForUser(ctx, user.UserID, core.RevokeCredentialsChanged))

	revoked := scalar[int64](t, e.Pool,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NOT NULL`, user.UserID)
	assert.Equal(t, int64(2), revoked)
	for _, id := range []uuid.UUID{first.SessionID, second.SessionID} {
		assert.Equal(t, string(core.RevokeCredentialsChanged), scalar[string](t, e.Pool,
			`SELECT revoked_reason FROM sessions WHERE id = $1`, id))
	}
}

// A login that read "MFA not required" before the factor was confirmed cannot
// create a password-only session afterwards: confirmation bumps the token
// version, so the stale version the login carries fails the CreateSession
// guard.
func TestMFALoginRace_PasswordOnlySessionRefusedAfterConfirm(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "mfa-race@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})

	// Step 1: the login verifies the password and reads the version it will
	// issue the session at (MFA is not required yet).
	member, err := e.Users.Authenticate(ctx, authn.LoginInput{Email: user.Email, Password: pgfixtures.Passphrase})
	require.NoError(t, err)
	required, err := e.MFA.Required(ctx, user.UserID)
	require.NoError(t, err)
	require.False(t, required, "the factor is not confirmed yet")

	// Step 2: a concurrent request confirms the factor.
	secret := pgfixtures.EnrollTOTPSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase, code)
	require.NoError(t, err)

	// Step 3: the first login tries to finish at the version it read.
	_, err = e.Sessions.Issue(ctx, user.UserID, member.User.TokenVersion, []string{core.AMRPassword})
	require.Error(t, err, "a password-only session must not survive an MFA enable")
	assert.Equal(t, 401, pgfixtures.AppStatus(err))
}

// CreateSession itself refuses a session without otp/rc in amr once the user
// has a confirmed factor, even when the caller passes the current version.
func TestCreateSession_RefusesPasswordOnlySessionOnceFactorConfirmed(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "factor-guard@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})
	secret := pgfixtures.EnrollTOTPSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, uuid.Nil, pgfixtures.Passphrase, code)
	require.NoError(t, err)

	version := scalar[int32](t, e.Pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID)

	_, err = e.Stores.Sessions.Create(ctx, core.NewSession{
		UserID: user.UserID, TokenVersion: version,
		ExpiresAt: time.Now().Add(time.Hour), AMR: []string{core.AMRPassword},
	})
	require.Error(t, err, "a password-only session must not be created for an MFA user")
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	created, err := e.Stores.Sessions.Create(ctx, core.NewSession{
		UserID: user.UserID, TokenVersion: version,
		ExpiresAt: time.Now().Add(time.Hour), AMR: []string{core.AMRPassword, core.AMROTP},
	})
	require.NoError(t, err)
	assert.Contains(t, created.AMR, core.AMROTP)
}

// Confirming a factor keeps the session that confirmed it (re-issued under the
// new token version with a fresh refresh-token family) while every other
// session, in the request tenant and in every other tenant, stops working at
// once.
func TestMFAConfirm_ReissuesCurrentSessionAndRejectsOtherTenants(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "mfa-confirm-acme")
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "mfa-confirm@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})
	testutil.CreateMember(t, ctx, e.Pool, acmeID, "mfa-confirm@example.com", testutil.MemberOptions{})

	current, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)
	other, err := e.Sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	secret := pgfixtures.EnrollTOTPSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	result, err := e.MFA.ConfirmTOTP(ctx, user.UserID, current.SessionID, pgfixtures.Passphrase, code)
	require.NoError(t, err)
	require.Equal(t, current.SessionID, result.Tokens.SessionID, "the confirming session is re-issued, not replaced")
	require.NotEmpty(t, result.Tokens.AccessToken)
	require.NotEmpty(t, result.Tokens.RefreshToken)

	newVersion := scalar[int32](t, e.Pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID)
	assert.Equal(t, user.TokenVersion+1, newVersion)
	assert.Equal(t, newVersion, scalar[int32](t, e.Pool,
		`SELECT token_version FROM sessions WHERE id = $1`, current.SessionID))
	assert.Contains(t, scalar[[]string](t, e.Pool,
		`SELECT amr FROM sessions WHERE id = $1`, current.SessionID), core.AMROTP)

	// The old access token carries the old version and is rejected; the
	// re-issued one (same row, new version) validates.
	require.Error(t, validateForUser(t, sessionValidator(e), tenant.DefaultID, user.UserID, current.SessionID, user.TokenVersion))
	require.NoError(t, validateForUser(t, sessionValidator(e), tenant.DefaultID, user.UserID, current.SessionID, newVersion))

	// The other tenant's session is rejected immediately and records why.
	require.Error(t, validateForUser(t, sessionValidator(e), acmeID, user.UserID, other.SessionID, user.TokenVersion))
	assert.Equal(t, string(core.RevokeMFAChanged), scalar[string](t, e.Pool,
		`SELECT revoked_reason FROM sessions WHERE id = $1`, other.SessionID))

	// The re-issued session replaced its refresh-token family: the old token
	// cannot refresh, the new one can.
	_, err = e.Sessions.Refresh(ctx, current.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, 401, pgfixtures.AppStatus(err))
	refreshed, err := e.Sessions.Refresh(ctx, result.Tokens.RefreshToken, nil)
	require.NoError(t, err)
	assert.Equal(t, current.SessionID, refreshed.Tokens.SessionID)
	assert.Equal(t, newVersion, refreshed.Member.User.TokenVersion)
}

// Disabling MFA keeps the session that disabled it and revokes the user's
// other sessions in every tenant through the same after-commit bypass
// statement.
func TestMFADisable_KeepsCurrentSessionAndRevokesOtherTenant(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "mfa-disable-acme")
	user := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "mfa-disable@example.com",
		testutil.MemberOptions{Password: pgfixtures.Passphrase})
	testutil.CreateMember(t, ctx, e.Pool, acmeID, "mfa-disable@example.com", testutil.MemberOptions{})

	current, err := e.Sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)
	other, err := e.Sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{core.AMRPassword})
	require.NoError(t, err)

	secret := pgfixtures.EnrollTOTPSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.MFA.ConfirmTOTP(ctx, user.UserID, current.SessionID, pgfixtures.Passphrase, code)
	require.NoError(t, err)

	// Confirmation already revoked the other session; open a new one in the
	// other tenant under the new version so disable has something to revoke.
	other, err = e.Sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, scalar[int32](t, e.Pool,
		`SELECT token_version FROM users WHERE id = $1`, user.UserID), []string{core.AMRPassword, core.AMROTP})
	require.NoError(t, err)

	code, err = totp.Code(secret, time.Now().Add(totp.Period))
	require.NoError(t, err)
	require.NoError(t, e.MFA.Disable(ctx, user.UserID, current.SessionID, code))

	assert.Nil(t, scalar[*time.Time](t, e.Pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, current.SessionID),
		"the session that disabled MFA stays live")
	assert.NotNil(t, scalar[*time.Time](t, e.Pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, other.SessionID),
		"the other tenant's session is revoked")
	assert.Equal(t, string(core.RevokeMFAChanged), scalar[string](t, e.Pool,
		`SELECT revoked_reason FROM sessions WHERE id = $1`, other.SessionID))
}
