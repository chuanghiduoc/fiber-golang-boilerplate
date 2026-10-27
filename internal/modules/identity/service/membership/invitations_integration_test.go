//go:build integration

package membership_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

func inviteForTest(t *testing.T, e *pgfixtures.Env, ctx context.Context, inviterID uuid.UUID, email string) string {
	t.Helper()
	invitations := membership.NewInvitationService(e.Stores, e.Tx, "https://app.example.test")
	_, err := invitations.Invite(ctx, membership.InviteInput{
		InviterID: inviterID, Email: email, Role: core.RoleMember,
	})
	require.NoError(t, err)
	return e.Mail.TokenFor(t, email)
}

func memberContextIn(tenantID, userID uuid.UUID, role string) context.Context {
	ctx := tenant.WithID(context.Background(), tenantID)
	granted := pgfixtures.Catalog.PermissionsForRole(role)
	permissions := make([]authz.Permission, 0, len(granted))
	for permission := range granted {
		permissions = append(permissions, permission)
	}
	return authz.WithSubject(ctx, authz.NewSubjectWithPermissions(userID, tenantID, role, permissions...))
}

// Accept runs its membership and audit work under the invitation's tenant
// scope, so a failure in the last (audit) step still rolls the join back. This
// guards the split's ordering: the membership insert must not commit before
// the audit row does.
func TestInvitationAccept_RollsBackWhenAuditFails(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviter := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rollback-inviter@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	otherTenant := testutil.CreateTenant(t, ctx, e.Pool, "rollback-other")
	invitee := testutil.CreateMember(t, ctx, e.Pool, otherTenant, "rollback-invitee@example.com", testutil.MemberOptions{})
	token := inviteForTest(t, e, memberContext(inviter.UserID, core.RoleOwner), inviter.UserID, invitee.Email)

	broken := identityrepo.NewTransactor(e.Fx.Tx(), nil, nil, nil, testutil.BanHasher(t)) // nil recorder fails closed
	refusing := membership.NewInvitationService(e.Stores, broken, "https://app.example.test")
	_, err := refusing.Accept(context.Background(), invitee.UserID, token)
	require.Error(t, err)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), pgfixtures.AppCode(err))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, invitee.UserID),
		"the membership must roll back with the missing audit row")
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM invitations WHERE email_normalized = $1 AND accepted_at IS NOT NULL`, invitee.Email),
		"the invitation must stay pending")
}

// The possession-based accept joins a non-default invitation tenant; the
// tenant-scoped lock/read carries an explicit tenant predicate and cannot
// borrow a fallback tenant.
func TestInvitationAccept_JoinsTheInvitingTenant(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviteTenant := testutil.CreateTenant(t, ctx, e.Pool, "join-target")
	otherTenant := testutil.CreateTenant(t, ctx, e.Pool, "join-other")
	inviter := testutil.CreateMember(t, ctx, e.Pool, inviteTenant, "join-inviter@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	invitee := testutil.CreateMember(t, ctx, e.Pool, otherTenant, "join-invitee@example.com", testutil.MemberOptions{})
	token := inviteForTest(t, e, memberContextIn(inviteTenant, inviter.UserID, core.RoleOwner), inviter.UserID, invitee.Email)

	invitations := membership.NewInvitationService(e.Stores, e.Tx, "https://app.example.test")
	// The request context has no tenant; the accept must use the invitation's.
	member, err := invitations.Accept(context.Background(), invitee.UserID, token)
	require.NoError(t, err)
	assert.Equal(t, inviteTenant, member.Membership.TenantID)
	assert.Equal(t, core.RoleMember, member.Membership.Role)
	assert.Equal(t, "active", scalar[string](t, e.Pool,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, inviteTenant, invitee.UserID))
}

// A token hash is never resolved under another tenant's RLS scope, so a
// foreign tenant reads it as not found even without the bypass.
func TestInvitationLookup_IsTenantScoped(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviteTenant := testutil.CreateTenant(t, ctx, e.Pool, "scope-target")
	otherTenant := testutil.CreateTenant(t, ctx, e.Pool, "scope-other")
	inviter := testutil.CreateMember(t, ctx, e.Pool, inviteTenant, "scope-inviter@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	invitee := testutil.CreateMember(t, ctx, e.Pool, otherTenant, "scope-invitee@example.com", testutil.MemberOptions{})
	inviteForTest(t, e, memberContextIn(inviteTenant, inviter.UserID, core.RoleOwner), inviter.UserID, invitee.Email)
	tokenHash := scalar[[]byte](t, e.Pool,
		`SELECT token_hash FROM invitations WHERE email_normalized = $1`, invitee.Email)

	_, err := e.Stores.Invitations.GetByTokenHashForUpdate(tenant.WithID(ctx, otherTenant), otherTenant, tokenHash)
	assert.ErrorIs(t, err, apperror.ErrNotFound, "another tenant must not see the invitation")

	inv, err := e.Stores.Invitations.GetByTokenHashForUpdate(tenant.WithID(ctx, inviteTenant), inviteTenant, tokenHash)
	require.NoError(t, err)
	assert.Equal(t, inviteTenant, inv.TenantID)
}
