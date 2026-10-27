//go:build integration

package authn_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// Registration is Tier 1: when the audit row cannot be written the user,
// password, and membership roll back together.
func TestRegister_RollsBackEverythingWhenAuditFails(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	broken := identityrepo.NewTransactor(e.Fx.Tx(), nil, nil, nil, testutil.BanHasher(t)) // nil recorder fails closed
	users := authn.NewUserService(authn.UserDeps{Stores: e.Stores, Tx: broken, Cache: cache.NewMemoryCache()})

	_, err := users.Register(ctx, authn.RegisterInput{Email: "atomic@example.com", Password: testPassphrase, Name: "Atomic"})
	require.Error(t, err)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), pgfixtures.AppCode(err))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM users`))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM memberships`))
	assert.Zero(t, scalar[int64](t, e.Pool, `SELECT count(*) FROM user_passwords`))
}

// The last active owner of a tenant can be neither demoted, banned, removed,
// nor deleted with their account; the trigger on memberships backs the
// service checks.
func TestLastOwnerProtection(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "acme")
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "owner@example.com", testutil.MemberOptions{Role: "owner"})
	// The same user is a plain member elsewhere, which must not count.
	testutil.CreateMember(t, ctx, e.Pool, acmeID, "owner@example.com", testutil.MemberOptions{Role: "member"})
	testutil.CreateMember(t, ctx, e.Pool, acmeID, "second@example.com", testutil.MemberOptions{Role: "owner"})

	_, err := e.Admin.UpdateRole(ctx, owner.UserID, core.RoleMember)
	assert.Equal(t, string(identitycatalog.CodeUserLastAdmin), pgfixtures.AppCode(err))
	assert.Equal(t, string(identitycatalog.CodeUserLastAdmin), pgfixtures.AppCode(e.Admin.BanUser(ctx, owner.UserID)))
	assert.Equal(t, string(identitycatalog.CodeUserLastAdmin), pgfixtures.AppCode(e.Users.DeleteAccount(ctx, owner.UserID)),
		"deleting the account would leave the default tenant without an owner")

	// Removing them from acme is fine: acme has another owner.
	require.NoError(t, e.Users.RemoveMember(tenant.WithID(ctx, acmeID), owner.UserID))
	assert.Equal(t, "removed", scalar[string](t, e.Pool,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, acmeID, owner.UserID))

	// The database trigger refuses a direct write too.
	_, err = e.Pool.Exec(ctx, `DELETE FROM membership_roles WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND user_id = $1`, owner.UserID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last owner")
}

// A ban is per membership and survives the banned account deleting itself:
// the address cannot register into (or be invited to) that tenant again.
func TestBanSurvivesAccountDeletion(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "admin@example.com", testutil.MemberOptions{Role: "admin"})
	user, err := e.Users.Register(ctx, authn.RegisterInput{Email: "banned@example.com", Password: testPassphrase, Name: "Banned"})
	require.NoError(t, err)
	require.NoError(t, e.Admin.BanUser(ctx, user.User.ID))
	assert.NotNil(t, scalar[*time.Time](t, e.Pool,
		`SELECT credentials_revoked_at FROM memberships WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND user_id = $1`, user.User.ID),
		"a ban revokes the member's API keys")

	// The banned user can still delete their global account (from another
	// tenant, where they are a member).
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "acme")
	testutil.CreateMember(t, ctx, e.Pool, acmeID, "banned@example.com", testutil.MemberOptions{})
	require.NoError(t, e.Users.DeleteAccount(tenant.WithID(ctx, acmeID), user.User.ID))
	assert.Equal(t, "suspended", scalar[string](t, e.Pool,
		`SELECT status FROM memberships WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND user_id = $1`, user.User.ID), "the ban stays recorded")

	_, err = e.Users.Register(ctx, authn.RegisterInput{Email: "banned@example.com", Password: testPassphrase, Name: "Again"})
	assert.Equal(t, http.StatusConflict, pgfixtures.AppStatus(err))
	again, err := e.Users.Register(tenant.WithID(ctx, acmeID), authn.RegisterInput{Email: "banned@example.com", Password: testPassphrase, Name: "Again"})
	require.NoError(t, err, "the address is free elsewhere once the account is deleted")
	assert.NotEqual(t, user.User.ID, again.User.ID)
}

// Two concurrent rotations of one refresh token: exactly one succeeds and the
// loser gets the concurrent-refresh code; the shared session stays live.
func TestRefresh_ConcurrentRotationKeepsFamily(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user, err := e.Users.Register(ctx, authn.RegisterInput{Email: "race@example.com", Password: testPassphrase, Name: "Race"})
	require.NoError(t, err)
	pair, err := e.Sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = e.Sessions.Refresh(ctx, pair.RefreshToken, nil)
		}()
	}
	wg.Wait()
	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			assert.Equal(t, string(identitycatalog.CodeAuthRefreshConcurrent), pgfixtures.AppCode(err))
		}
	}
	assert.Equal(t, 1, succeeded, "a refresh token rotates once")
	assert.Equal(t, int64(1), scalar[int64](t, e.Pool,
		`SELECT count(*) FROM refresh_tokens WHERE session_id = $1 AND used_at IS NOT NULL`, pair.SessionID))
	assert.Nil(t, scalar[*time.Time](t, e.Pool,
		`SELECT revoked_at FROM sessions WHERE id = $1`, pair.SessionID),
		"the concurrent loser must not revoke the session")
}

// Password reset is per user: the token works without a tenant, revokes every
// session in every tenant, and later account changes invalidate older links.
func TestPasswordReset_IsGlobal(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "acme")
	user, err := e.Users.Register(ctx, authn.RegisterInput{Email: "reset@example.com", Password: testPassphrase, Name: "Reset"})
	require.NoError(t, err)
	testutil.CreateMember(t, ctx, e.Pool, acmeID, "reset@example.com", testutil.MemberOptions{})
	inDefault, err := e.Sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)
	inAcme, err := e.Sessions.Issue(tenant.WithID(ctx, acmeID), user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	require.NoError(t, e.Resets.ForgotPassword(tenant.WithIdentity(ctx, acmeID, "acme"), authn.ForgotPasswordInput{Email: "Reset@Example.com"}))
	resetToken := e.Mail.TokenFor(t, "reset@example.com")

	// Reset from a request in the default tenant: the link is not tenant-bound.
	require.NoError(t, e.Resets.ResetPassword(ctx, authn.ResetPasswordInput{Token: resetToken, Password: testNewPassword}))
	for _, sid := range []uuid.UUID{inDefault.SessionID, inAcme.SessionID} {
		assert.Equal(t, "credentials_changed", scalar[string](t, e.Pool, `SELECT revoked_reason FROM sessions WHERE id = $1`, sid))
	}
	_, err = e.Users.Authenticate(tenant.WithID(ctx, acmeID), authn.LoginInput{Email: "reset@example.com", Password: testNewPassword})
	assert.NoError(t, err, "the new password works in every tenant")
	assert.Equal(t, http.StatusBadRequest, pgfixtures.AppStatus(e.Resets.ResetPassword(ctx, authn.ResetPasswordInput{Token: resetToken, Password: strings.Join([]string{"second", "reset", "attempt", "phrase"}, "-")})),
		"single use")
}

func TestEmailVerification_IsPerUser(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user, err := e.Users.Register(ctx, authn.RegisterInput{Email: "verify@example.com", Password: testPassphrase, Name: "V"})
	require.NoError(t, err)
	require.NoError(t, e.Verify.SendVerification(tenant.WithIdentity(ctx, tenant.DefaultID, "default"), user.User.ID, user.User.Email))
	tok := e.Mail.TokenFor(t, "verify@example.com")

	acmeID := testutil.CreateTenant(t, ctx, e.Pool, "acme")
	require.NoError(t, e.Verify.Verify(tenant.WithID(ctx, acmeID), tok), "verification is not tenant-bound")
	assert.NotNil(t, scalar[*time.Time](t, e.Pool, `SELECT email_verified_at FROM users WHERE id = $1`, user.User.ID))
	assert.Equal(t, http.StatusBadRequest, pgfixtures.AppStatus(e.Verify.Verify(ctx, tok)), "single use")
}
