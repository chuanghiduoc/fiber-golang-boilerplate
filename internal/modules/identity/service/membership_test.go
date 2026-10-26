package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func codeOf(err error) string {
	if appErr, ok := err.(*apperror.AppError); ok {
		return appErr.Code
	}
	return ""
}

// A global user is a member of the tenant it registered in only. A correct
// password in another tenant is the same 401 as a wrong one, and
// registering the same address there is refused: joining another tenant
// takes an invitation.
func TestRegistrationAndLogin_AreScopedToMemberships(t *testing.T) {
	db := newMemDB()
	svc := newTestUserService(db)
	ctx := context.Background()
	acme := tenant.WithID(ctx, 2)

	user, err := svc.Register(ctx, RegisterInput{Email: "Ada@Example.com", Password: "Password1!", Name: "Ada"})
	require.NoError(t, err)
	assert.Equal(t, "ada@example.com", user.User.Email)
	assert.Equal(t, int64(1), user.Membership.TenantID)
	assert.Equal(t, RoleMember, user.Membership.Role)
	assert.Equal(t, string(MembershipStatusActive), string(user.Membership.Status))

	_, err = svc.Authenticate(acme, LoginInput{Email: "ada@example.com", Password: "Password1!"})
	assert.Equal(t, string(identitycatalog.CodeAuthInvalidCredentials), codeOf(err), "no membership in tenant 2")

	_, err = svc.Register(acme, RegisterInput{Email: "ada@example.com", Password: "Password1!", Name: "Ada again"})
	assert.Equal(t, string(identitycatalog.CodeUserEmailTaken), codeOf(err))
	assert.Len(t, db.users, 1, "no second account is created")
}

func TestAuthenticate_BannedMemberGetsInvalidCredentials(t *testing.T) {
	db := newMemDB()
	svc := newTestUserService(db)
	ctx := context.Background()
	user, err := svc.Register(ctx, RegisterInput{Email: "b@example.com", Password: "Password1!", Name: "B"})
	require.NoError(t, err)
	_, err = db.stores().Memberships.Ban(ctx, user.User.ID)
	require.NoError(t, err)

	_, err = svc.Authenticate(ctx, LoginInput{Email: "b@example.com", Password: "Password1!"})
	assert.Equal(t, string(identitycatalog.CodeAuthInvalidCredentials), codeOf(err))

	// The ban survives in the tenant: the address cannot register there again
	// (here it still has a live account, so it is simply taken).
	_, err = svc.Register(ctx, RegisterInput{Email: "b@example.com", Password: "Password1!", Name: "B"})
	assert.Equal(t, http.StatusConflict, statusOf(err))
}

func newTestSessions(db *memDB, signed *[]AccessClaims) *SessionService {
	sign := func(c AccessClaims) (string, error) {
		*signed = append(*signed, c)
		return "access", nil
	}
	return NewSessionService(db.stores(), memTx{db}, sign, time.Hour, 720*time.Hour, nil)
}

func TestSessions_RotationAndReuseRevokesFamily(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "s@example.com", Password: "Password1!", Name: "S"})
	require.NoError(t, err)

	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	first, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)
	require.Positive(t, first.SessionID)
	require.Len(t, signed, 1)
	assert.Equal(t, AccessClaims{UserID: user.User.ID, TenantID: 1, SessionID: first.SessionID, Email: "s@example.com", AMR: []string{AMRPassword}}, signed[0])

	rotated, err := sessions.Refresh(ctx, first.RefreshToken, nil)
	require.NoError(t, err)
	assert.Equal(t, first.SessionID, rotated.Tokens.SessionID, "rotation keeps the session (family)")
	assert.NotEqual(t, first.RefreshToken, rotated.Tokens.RefreshToken)

	// Replaying the consumed token is reuse: 401 and the whole family dies,
	// including the freshly rotated token.
	_, err = sessions.Refresh(ctx, first.RefreshToken, nil)
	assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), codeOf(err))
	assert.Equal(t, RevokeRefreshReuse, db.sessions[first.SessionID].RevokedReason)
	_, err = sessions.Refresh(ctx, rotated.Tokens.RefreshToken, nil)
	assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), codeOf(err))
}

func TestSessions_CrossTenantRefreshHasNoSideEffects(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "x@example.com", Password: "Password1!", Name: "X"})
	require.NoError(t, err)
	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	_, err = sessions.Refresh(tenant.WithID(ctx, 2), pair.RefreshToken, nil)
	assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), codeOf(err))
	assert.Nil(t, db.sessions[pair.SessionID].RevokedAt, "another tenant cannot revoke the session")

	_, err = sessions.Refresh(ctx, pair.RefreshToken, nil)
	assert.NoError(t, err, "the token is still usable in its own tenant")
}

func TestSessions_AdmissionRefusalDoesNotConsume(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "a@example.com", Password: "Password1!", Name: "A"})
	require.NoError(t, err)
	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	refuse := func(context.Context, *Member) error { return apperror.NewForbidden("not yet") }
	_, err = sessions.Refresh(ctx, pair.RefreshToken, refuse)
	assert.Equal(t, http.StatusForbidden, statusOf(err))
	_, err = sessions.Refresh(ctx, pair.RefreshToken, nil)
	assert.NoError(t, err, "a refused refresh must not burn (or reuse-revoke) the token")
}

func TestSessions_StaleTokenVersionCannotIssueOrRefresh(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	svc := newTestUserService(db)
	user, err := svc.Register(ctx, RegisterInput{Email: "v@example.com", Password: "Password1!", Name: "V"})
	require.NoError(t, err)
	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	// logout-all bumps the version and revokes every session.
	require.NoError(t, svc.RevokeSessions(ctx, user.User.ID))
	_, err = sessions.Refresh(ctx, pair.RefreshToken, nil)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))
	_, err = sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err), "a password checked at the old version cannot open a session")
}

func TestSessions_RefreshTokenNeverOutlivesSession(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "life@example.com", Password: "Password1!", Name: "L"})
	require.NoError(t, err)
	sessionLifetime := 2 * time.Hour
	sessions := NewSessionService(db.stores(), memTx{db}, func(AccessClaims) (string, error) { return "access", nil }, 30*24*time.Hour, sessionLifetime, nil)
	pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	sess := db.sessions[pair.SessionID]
	assert.WithinDuration(t, time.Now().Add(sessionLifetime), sess.ExpiresAt, time.Minute)

	token, err := db.stores().RefreshTokens.GetByHash(ctx, hashToken(pair.RefreshToken))
	require.NoError(t, err)
	assert.False(t, token.ExpiresAt.After(sess.ExpiresAt), "the refresh TTL must be capped by the session lifetime")
}

func TestSessions_ExpiredSessionCannotRefresh(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	user, err := newTestUserService(db).Register(ctx, RegisterInput{Email: "expired@example.com", Password: "Password1!", Name: "E"})
	require.NoError(t, err)
	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	db.mu.Lock()
	db.sessions[pair.SessionID].ExpiresAt = time.Now().Add(-time.Second)
	db.mu.Unlock()

	_, err = sessions.Refresh(ctx, pair.RefreshToken, nil)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))
	assert.Empty(t, signed[1:], "an expired session must not mint an access token")
}

func TestAdmin_BanIsPerTenantAndProtectsLastOwner(t *testing.T) {
	db := newMemDB()
	ctx := context.Background()
	st := db.stores()
	admin := seedMember(t, db, 1, "admin@example.com", RoleOwner)
	member := seedMember(t, db, 1, "member@example.com", RoleMember)
	_, err := st.Memberships.Create(tenant.WithID(ctx, 2), member, RoleMember)
	require.NoError(t, err)

	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	inDefault, err := sessions.Issue(ctx, member, 0, nil)
	require.NoError(t, err)
	inAcme, err := sessions.Issue(tenant.WithID(ctx, 2), member, 0, nil)
	require.NoError(t, err)

	adminSvc := NewAdminUserService(st, memTx{db}, nil)
	systemCtx := authz.SystemContext(ctx)
	err = adminSvc.BanUser(systemCtx, admin)
	assert.Equal(t, string(apperror.CodeUserLastAdmin), codeOf(err))

	require.NoError(t, adminSvc.BanUser(systemCtx, member))
	assert.Equal(t, RevokeMembershipChanged, db.sessions[inDefault.SessionID].RevokedReason)
	assert.Nil(t, db.sessions[inAcme.SessionID].RevokedAt, "a ban in one tenant leaves the member's other tenants alone")
	acmeMember, err := st.Memberships.GetActive(tenant.WithID(ctx, 2), member)
	require.NoError(t, err)
	assert.True(t, acmeMember.Active())

	restored, err := adminSvc.UnbanUser(systemCtx, member)
	require.NoError(t, err)
	assert.Equal(t, MembershipStatusActive, restored.Membership.Status)
	assert.Nil(t, restored.Membership.BannedAt)
}

func seedMember(t *testing.T, db *memDB, tenantID int64, email, role string) int64 {
	t.Helper()
	ctx := tenant.WithID(context.Background(), tenantID)
	st := db.stores()
	user, err := st.Users.Create(ctx, NewUser{Email: email, Name: email, EmailVerified: true})
	require.NoError(t, err)
	_, err = st.Memberships.Create(ctx, user.ID, role)
	require.NoError(t, err)
	return user.ID
}

// deleteAccount must take the membership row lock before the per-tenant
// owner-change advisory lock, the same order rank-checked mutations use; the
// reverse order deadlocks with a concurrent ban or demotion.
func TestDeleteAccountLocksMembershipBeforeOwnerChanges(t *testing.T) {
	db := newMemDB()
	owner := seedMember(t, db, tenant.DefaultID, "lock-order@example.com", RoleOwner)
	st := db.stores()
	var order []string
	st.Memberships = lockOrderMemberships{MembershipRepository: st.Memberships, order: &order}

	err := deleteAccount(context.Background(), st, owner)
	assert.Equal(t, string(apperror.CodeUserLastAdmin), codeOf(err))
	assert.Equal(t, []string{"lock-membership", "lock-owner-changes"}, order)
}

type lockOrderMemberships struct {
	MembershipRepository
	order *[]string
}

func (r lockOrderMemberships) LockMembership(ctx context.Context, userID int64) error {
	*r.order = append(*r.order, "lock-membership")
	return r.MembershipRepository.LockMembership(ctx, userID)
}

func (r lockOrderMemberships) LockOwnerChanges(ctx context.Context) error {
	*r.order = append(*r.order, "lock-owner-changes")
	return r.MembershipRepository.LockOwnerChanges(ctx)
}

func TestMemberUpdatedAtAndDeletedAt(t *testing.T) {
	early, late := time.Unix(100, 0), time.Unix(200, 0)
	removed, deleted := time.Unix(300, 0), time.Unix(400, 0)
	m := Member{User: User{UpdatedAt: early}, Membership: Membership{UpdatedAt: late}}
	assert.Equal(t, late, m.UpdatedAt(), "the later of user and membership")
	m.User.UpdatedAt, m.Membership.UpdatedAt = late, early
	assert.Equal(t, late, m.UpdatedAt())

	assert.Nil(t, m.DeletedAt())
	m.User.DeletedAt = &deleted
	assert.Equal(t, &deleted, m.DeletedAt(), "account deletion when still a member")
	m.Membership.RemovedAt = &removed
	assert.Equal(t, &removed, m.DeletedAt(), "leaving the tenant wins")
}
