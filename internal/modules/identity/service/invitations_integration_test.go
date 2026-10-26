//go:build integration

package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func inviteForTest(t *testing.T, e *env, ctx context.Context, inviterID int64, email string) string {
	t.Helper()
	invitations := identityservice.NewInvitationService(e.stores, e.tx, e.mail, "https://app.example.test")
	_, err := invitations.Invite(ctx, identityservice.InviteInput{
		InviterID: inviterID, Email: email, Role: identityservice.RoleMember,
	})
	require.NoError(t, err)
	return e.mail.tokenFor(t, email)
}

func memberContextIn(tenantID, userID int64, role string) context.Context {
	ctx := tenant.WithID(context.Background(), tenantID)
	return authz.WithSubject(ctx, authz.NewSubject(userID, tenantID, role))
}

// Accept runs its membership and audit work under the invitation's tenant
// scope, so a failure in the last (audit) step still rolls the join back. This
// guards the split's ordering: the membership insert must not commit before
// the audit row does.
func TestInvitationAccept_RollsBackWhenAuditFails(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviter := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rollback-inviter@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	otherTenant := testutil.CreateTenant(t, ctx, e.pool, "rollback-other")
	invitee := testutil.CreateMember(t, ctx, e.pool, otherTenant, "rollback-invitee@example.com", testutil.MemberOptions{})
	token := inviteForTest(t, e, memberContext(inviter.UserID, identityservice.RoleOwner), inviter.UserID, invitee.Email)

	broken := identityrepo.NewTransactor(testutil.NewTxManager(e.pool), nil, nil, nil) // nil recorder fails closed
	refusing := identityservice.NewInvitationService(e.stores, broken, e.mail, "https://app.example.test")
	_, err := refusing.Accept(context.Background(), invitee.UserID, token)
	require.Error(t, err)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), appCode(err))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, invitee.UserID),
		"the membership must roll back with the missing audit row")
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM invitations WHERE email_normalized = $1 AND accepted_at IS NOT NULL`, invitee.Email),
		"the invitation must stay pending")
}

// The possession-based accept joins a non-default invitation tenant; the
// tenant-scoped lock/read carries an explicit tenant predicate, so it cannot
// use the tenant-1 fallback.
func TestInvitationAccept_JoinsTheInvitingTenant(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviteTenant := testutil.CreateTenant(t, ctx, e.pool, "join-target")
	otherTenant := testutil.CreateTenant(t, ctx, e.pool, "join-other")
	inviter := testutil.CreateMember(t, ctx, e.pool, inviteTenant, "join-inviter@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	invitee := testutil.CreateMember(t, ctx, e.pool, otherTenant, "join-invitee@example.com", testutil.MemberOptions{})
	token := inviteForTest(t, e, memberContextIn(inviteTenant, inviter.UserID, identityservice.RoleOwner), inviter.UserID, invitee.Email)

	invitations := identityservice.NewInvitationService(e.stores, e.tx, e.mail, "https://app.example.test")
	// The request context has no tenant; the accept must use the invitation's.
	member, err := invitations.Accept(context.Background(), invitee.UserID, token)
	require.NoError(t, err)
	assert.Equal(t, inviteTenant, member.Membership.TenantID)
	assert.Equal(t, identityservice.RoleMember, member.Membership.Role)
	assert.Equal(t, "active", scalar[string](t, e.pool,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, inviteTenant, invitee.UserID))
}

// A token hash is never resolved under another tenant's RLS scope, so a
// foreign tenant reads it as not found even without the bypass.
func TestInvitationLookup_IsTenantScoped(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviteTenant := testutil.CreateTenant(t, ctx, e.pool, "scope-target")
	otherTenant := testutil.CreateTenant(t, ctx, e.pool, "scope-other")
	inviter := testutil.CreateMember(t, ctx, e.pool, inviteTenant, "scope-inviter@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	invitee := testutil.CreateMember(t, ctx, e.pool, otherTenant, "scope-invitee@example.com", testutil.MemberOptions{})
	inviteForTest(t, e, memberContextIn(inviteTenant, inviter.UserID, identityservice.RoleOwner), inviter.UserID, invitee.Email)
	tokenHash := scalar[string](t, e.pool,
		`SELECT token_hash FROM invitations WHERE email_normalized = $1`, invitee.Email)

	_, err := e.stores.Invitations.GetByTokenHashForUpdate(tenant.WithID(ctx, otherTenant), otherTenant, tokenHash)
	assert.ErrorIs(t, err, apperror.ErrNotFound, "another tenant must not see the invitation")

	inv, err := e.stores.Invitations.GetByTokenHashForUpdate(tenant.WithID(ctx, inviteTenant), inviteTenant, tokenHash)
	require.NoError(t, err)
	assert.Equal(t, inviteTenant, inv.TenantID)
}
