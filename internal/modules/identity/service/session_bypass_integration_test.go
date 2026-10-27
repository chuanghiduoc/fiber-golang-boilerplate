//go:build integration

package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
	"github.com/google/uuid"
)

// sessionValidator is the real access-token validator over the application
// stores (no cache, so every call reads the database).
func sessionValidator(e *env) session.Store {
	return session.NewStore(identityadapter.NewSessionStateReader(e.stores.Sessions), nil)
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
	e := newEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "pw-change-acme")
	password := strings.Join([]string{"correct", "horse", "battery", "staple"}, "-")
	user := testutil.CreateMember(t, ctx, e.pool, acmeID, "pw-cross@example.com",
		testutil.MemberOptions{Password: password})
	testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "pw-cross@example.com", testutil.MemberOptions{})

	other, err := e.sessions.Issue(tenant.WithID(ctx, tenant.DefaultID), user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	newPassword := strings.Join([]string{"another", "horse", "battery", "staple"}, "-")
	require.NoError(t, e.users.ChangePassword(tenant.WithID(ctx, acmeID), user.UserID, service.ChangePasswordInput{
		CurrentPassword: password,
		NewPassword:     newPassword,
	}))

	// The other tenant's access token is rejected right after the change.
	err = validateForUser(t, sessionValidator(e), tenant.DefaultID, user.UserID, other.SessionID, user.TokenVersion)
	require.Error(t, err)
	assert.Equal(t, 401, appStatus(err))

	// Its refresh token cannot mint a new one either.
	_, err = e.sessions.Refresh(tenant.WithID(ctx, tenant.DefaultID), other.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, 401, appStatus(err))

	// The follow-up bypass statement recorded the revocation and its reason.
	assert.Equal(t, string(service.RevokeCredentialsChanged), scalar[string](t, e.pool,
		`SELECT revoked_reason FROM sessions WHERE id = $1`, other.SessionID))
	assert.NotNil(t, scalar[*time.Time](t, e.pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, other.SessionID))
}

// The cleanup the credential/MFA changes run after commit only touches rows
// whose token version no longer matches: running it twice is idempotent.
func TestRevokeStaleUserSessions_CleanupIsIdempotent(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "cleanup-idempotent@example.com", testutil.MemberOptions{})

	first, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)
	second, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	// A credential change bumps the version; the sessions are stale.
	_, err = e.pool.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id = $1`, user.UserID)
	require.NoError(t, err)

	require.NoError(t, e.stores.Sessions.RevokeStaleForUser(ctx, user.UserID, service.RevokeCredentialsChanged))
	require.NoError(t, e.stores.Sessions.RevokeStaleForUser(ctx, user.UserID, service.RevokeCredentialsChanged))

	revoked := scalar[int64](t, e.pool,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NOT NULL`, user.UserID)
	assert.Equal(t, int64(2), revoked)
	for _, id := range []uuid.UUID{first.SessionID, second.SessionID} {
		assert.Equal(t, string(service.RevokeCredentialsChanged), scalar[string](t, e.pool,
			`SELECT revoked_reason FROM sessions WHERE id = $1`, id))
	}
}

// A login that read "MFA not required" before the factor was confirmed cannot
// create a password-only session afterwards: confirmation bumps the token
// version, so the stale version the login carries fails the CreateSession
// guard.
func TestMFALoginRace_PasswordOnlySessionRefusedAfterConfirm(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "mfa-race@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})

	// Step 1: the login verifies the password and reads the version it will
	// issue the session at (MFA is not required yet).
	member, err := e.users.Authenticate(ctx, service.LoginInput{Email: user.Email, Password: mfaIntegrationPassword})
	require.NoError(t, err)
	required, err := e.mfa.Required(ctx, user.UserID)
	require.NoError(t, err)
	require.False(t, required, "the factor is not confirmed yet")

	// Step 2: a concurrent request confirms the factor.
	secret := enrollSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, uuid.Nil, mfaIntegrationPassword, code)
	require.NoError(t, err)

	// Step 3: the first login tries to finish at the version it read.
	_, err = e.sessions.Issue(ctx, user.UserID, member.User.TokenVersion, []string{service.AMRPassword})
	require.Error(t, err, "a password-only session must not survive an MFA enable")
	assert.Equal(t, 401, appStatus(err))
}

// CreateSession itself refuses a session without otp/rc in amr once the user
// has a confirmed factor, even when the caller passes the current version.
func TestCreateSession_RefusesPasswordOnlySessionOnceFactorConfirmed(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "factor-guard@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})
	secret := enrollSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, uuid.Nil, mfaIntegrationPassword, code)
	require.NoError(t, err)

	version := scalar[int32](t, e.pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID)

	_, err = e.stores.Sessions.Create(ctx, service.NewSession{
		UserID: user.UserID, TokenVersion: version,
		ExpiresAt: time.Now().Add(time.Hour), AMR: []string{service.AMRPassword},
	})
	require.Error(t, err, "a password-only session must not be created for an MFA user")
	assert.True(t, errors.Is(err, apperror.ErrNotFound))

	created, err := e.stores.Sessions.Create(ctx, service.NewSession{
		UserID: user.UserID, TokenVersion: version,
		ExpiresAt: time.Now().Add(time.Hour), AMR: []string{service.AMRPassword, service.AMROTP},
	})
	require.NoError(t, err)
	assert.Contains(t, created.AMR, service.AMROTP)
}

// Confirming a factor keeps the session that confirmed it (re-issued under the
// new token version with a fresh refresh-token family) while every other
// session, in the request tenant and in every other tenant, stops working at
// once.
func TestMFAConfirm_ReissuesCurrentSessionAndRejectsOtherTenants(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "mfa-confirm-acme")
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "mfa-confirm@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})
	testutil.CreateMember(t, ctx, e.pool, acmeID, "mfa-confirm@example.com", testutil.MemberOptions{})

	current, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)
	other, err := e.sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	secret := enrollSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	result, err := e.mfa.ConfirmTOTP(ctx, user.UserID, current.SessionID, mfaIntegrationPassword, code)
	require.NoError(t, err)
	require.Equal(t, current.SessionID, result.Tokens.SessionID, "the confirming session is re-issued, not replaced")
	require.NotEmpty(t, result.Tokens.AccessToken)
	require.NotEmpty(t, result.Tokens.RefreshToken)

	newVersion := scalar[int32](t, e.pool, `SELECT token_version FROM users WHERE id = $1`, user.UserID)
	assert.Equal(t, user.TokenVersion+1, newVersion)
	assert.Equal(t, newVersion, scalar[int32](t, e.pool,
		`SELECT token_version FROM sessions WHERE id = $1`, current.SessionID))
	assert.Contains(t, scalar[[]string](t, e.pool,
		`SELECT amr FROM sessions WHERE id = $1`, current.SessionID), service.AMROTP)

	// The old access token carries the old version and is rejected; the
	// re-issued one (same row, new version) validates.
	require.Error(t, validateForUser(t, sessionValidator(e), tenant.DefaultID, user.UserID, current.SessionID, user.TokenVersion))
	require.NoError(t, validateForUser(t, sessionValidator(e), tenant.DefaultID, user.UserID, current.SessionID, newVersion))

	// The other tenant's session is rejected immediately and records why.
	require.Error(t, validateForUser(t, sessionValidator(e), acmeID, user.UserID, other.SessionID, user.TokenVersion))
	assert.Equal(t, string(service.RevokeMFAChanged), scalar[string](t, e.pool,
		`SELECT revoked_reason FROM sessions WHERE id = $1`, other.SessionID))

	// The re-issued session replaced its refresh-token family: the old token
	// cannot refresh, the new one can.
	_, err = e.sessions.Refresh(ctx, current.RefreshToken, nil)
	require.Error(t, err)
	assert.Equal(t, 401, appStatus(err))
	refreshed, err := e.sessions.Refresh(ctx, result.Tokens.RefreshToken, nil)
	require.NoError(t, err)
	assert.Equal(t, current.SessionID, refreshed.Tokens.SessionID)
	assert.Equal(t, newVersion, refreshed.Member.User.TokenVersion)
}

// Disabling MFA keeps the session that disabled it and revokes the user's
// other sessions in every tenant through the same after-commit bypass
// statement.
func TestMFADisable_KeepsCurrentSessionAndRevokesOtherTenant(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "mfa-disable-acme")
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "mfa-disable@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})
	testutil.CreateMember(t, ctx, e.pool, acmeID, "mfa-disable@example.com", testutil.MemberOptions{})

	current, err := e.sessions.Issue(ctx, user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)
	other, err := e.sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, user.TokenVersion, []string{service.AMRPassword})
	require.NoError(t, err)

	secret := enrollSecret(t, e, user.UserID)
	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, current.SessionID, mfaIntegrationPassword, code)
	require.NoError(t, err)

	// Confirmation already revoked the other session; open a new one in the
	// other tenant under the new version so disable has something to revoke.
	other, err = e.sessions.Issue(tenant.WithID(ctx, acmeID), user.UserID, scalar[int32](t, e.pool,
		`SELECT token_version FROM users WHERE id = $1`, user.UserID), []string{service.AMRPassword, service.AMROTP})
	require.NoError(t, err)

	code, err = totp.Code(secret, time.Now().Add(totp.Period))
	require.NoError(t, err)
	require.NoError(t, e.mfa.Disable(ctx, user.UserID, current.SessionID, code))

	assert.Nil(t, scalar[*time.Time](t, e.pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, current.SessionID),
		"the session that disabled MFA stays live")
	assert.NotNil(t, scalar[*time.Time](t, e.pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, other.SessionID),
		"the other tenant's session is revoked")
	assert.Equal(t, string(service.RevokeMFAChanged), scalar[string](t, e.pool,
		`SELECT revoked_reason FROM sessions WHERE id = $1`, other.SessionID))
}
