package repository

import (
	"context"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const uniqueMembershipsTenantUser = "uq_memberships_tenant_user"

// MembershipRepository implements service.MembershipRepository. Every query
// is scoped to the request tenant except ListForUser.
type MembershipRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.MembershipRepository = (*MembershipRepository)(nil)

func NewMembershipRepository(db identitysqlc.DBTX) *MembershipRepository {
	return &MembershipRepository{q: identitysqlc.New(db)}
}

func toMembership(row identitysqlc.Membership, role string, roles []string) identityservice.Membership {
	return identityservice.Membership{
		ID:                   row.ID,
		TenantID:             row.TenantID,
		UserID:               row.UserID,
		Role:                 role,
		Roles:                roles,
		Status:               identityservice.MembershipStatus(row.Status),
		BannedAt:             timePtr(row.BannedAt),
		BannedEmailHash:      row.BannedEmailHash.String,
		ErasureRequestedAt:   timePtr(row.ErasureRequestedAt),
		RemovedAt:            timePtr(row.RemovedAt),
		CredentialsRevokedAt: timePtr(row.CredentialsRevokedAt),
		CreatedAt:            row.CreatedAt.Time,
		UpdatedAt:            row.UpdatedAt.Time,
	}
}

func toMember(user identitysqlc.User, membership identitysqlc.Membership, role string,
	roles []string, hasPassword bool,
) identityservice.Member {
	return identityservice.Member{
		User:       *toUser(user, hasPassword),
		Membership: toMembership(membership, role, roles),
	}
}

func (r *MembershipRepository) Get(ctx context.Context, userID int64) (*identityservice.Member, error) {
	row, err := r.q.GetMember(ctx, identitysqlc.GetMemberParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMember(row.User, row.Membership, row.Role, row.Roles, row.HasPassword)
	return &m, nil
}

func (r *MembershipRepository) GetActive(ctx context.Context, userID int64) (*identityservice.Member, error) {
	row, err := r.q.GetActiveMember(ctx, identitysqlc.GetActiveMemberParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMember(row.User, row.Membership, row.Role, row.Roles, row.HasPassword)
	return &m, nil
}

func (r *MembershipRepository) ListActiveCursor(ctx context.Context, cur pagination.DBCursor) ([]identityservice.Member, error) {
	rows, err := r.q.ListActiveMembersCursor(ctx, identitysqlc.ListActiveMembersCursorParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	members := make([]identityservice.Member, len(rows))
	for i := range rows {
		members[i] = toMember(rows[i].User, rows[i].Membership, rows[i].Role, rows[i].Roles, rows[i].HasPassword)
	}
	return members, nil
}

func (r *MembershipRepository) ListCursor(ctx context.Context, cur pagination.DBCursor) ([]identityservice.Member, error) {
	rows, err := r.q.ListMembersCursor(ctx, identitysqlc.ListMembersCursorParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	members := make([]identityservice.Member, len(rows))
	for i := range rows {
		members[i] = toMember(rows[i].User, rows[i].Membership, rows[i].Role, rows[i].Roles, rows[i].HasPassword)
	}
	return members, nil
}

func (r *MembershipRepository) ListForUser(ctx context.Context, userID int64) ([]identityservice.Membership, error) {
	ctx = database.WithRLSBypass(ctx) // The user's memberships in every tenant (global account operations).
	rows, err := r.q.ListUserMemberships(ctx, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	memberships := make([]identityservice.Membership, len(rows))
	for i := range rows {
		memberships[i] = toMembership(rows[i].Membership, rows[i].Role, rows[i].Roles)
	}
	return memberships, nil
}

func (r *MembershipRepository) Create(ctx context.Context, userID int64, role string) (*identityservice.Membership, error) {
	row, err := r.q.CreateMembership(ctx, identitysqlc.CreateMembershipParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == uniqueMembershipsTenantUser {
			return nil, identityservice.ErrAlreadyMember
		}
		return nil, wrapErr(err)
	}
	if err := r.insertRole(ctx, userID, role); err != nil {
		return nil, err
	}
	m := toMembership(row, role, []string{role})
	return &m, nil
}

func (r *MembershipRepository) Reactivate(ctx context.Context, userID int64, role string) (*identityservice.Membership, error) {
	row, err := r.q.ReactivateMembership(ctx, identitysqlc.ReactivateMembershipParams{
		TenantID: tenantID(ctx), UserID: userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	if err := r.replaceRoles(ctx, userID, []string{role}); err != nil {
		return nil, err
	}
	m := toMembership(row, role, []string{role})
	return &m, nil
}

func (r *MembershipRepository) UpdateRole(ctx context.Context, userID int64, role string) (*identityservice.Membership, error) {
	member, err := r.GetActive(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := r.replaceRoles(ctx, userID, []string{role}); err != nil {
		return nil, err
	}
	member.Membership.Role = role
	member.Membership.Roles = []string{role}
	return &member.Membership, nil
}

func (r *MembershipRepository) Ban(ctx context.Context, userID int64) (*identityservice.Membership, error) {
	row, err := r.q.BanMembership(ctx, identitysqlc.BanMembershipParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	if err := r.deleteInvitationsCreatedBy(ctx, userID); err != nil {
		return nil, err
	}
	return r.hydrate(ctx, row)
}

func (r *MembershipRepository) Unban(ctx context.Context, userID int64) (*identityservice.Membership, error) {
	row, err := r.q.UnbanMembership(ctx, identitysqlc.UnbanMembershipParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return r.hydrate(ctx, row)
}

func (r *MembershipRepository) Remove(ctx context.Context, userID int64) (*identityservice.Membership, error) {
	row, err := r.q.RemoveMembership(ctx, identitysqlc.RemoveMembershipParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	// Removed memberships lose their roles, so a tenant role they held can be
	// deleted without waiting for a re-invitation.
	if err := r.q.DeleteMemberRoles(ctx, identitysqlc.DeleteMemberRolesParams{
		TenantID: tenantID(ctx), UserID: userID,
	}); err != nil {
		return nil, wrapErr(err)
	}
	if err := r.deleteInvitationsCreatedBy(ctx, userID); err != nil {
		return nil, err
	}
	return r.hydrate(ctx, row)
}

// SuspendForErasure marks a non-removed membership as erasure-pending. It
// leaves the ban columns alone; the trigger still protects the last owner.
func (r *MembershipRepository) SuspendForErasure(ctx context.Context, userID int64) error {
	rows, err := r.q.SuspendMembershipForErasure(ctx, identitysqlc.SuspendMembershipForErasureParams{
		TenantID: tenantID(ctx), UserID: userID,
	})
	if err != nil {
		return wrapErr(err)
	}
	if rows == 0 {
		return apperror.ErrNotFound
	}
	return nil
}

// deleteInvitationsCreatedBy revokes the pending invitations a banned or
// removed member created in the request tenant. It runs in the membership
// mutation's transaction so an invitation cannot outlive its inviter's
// authority.
func (r *MembershipRepository) deleteInvitationsCreatedBy(ctx context.Context, userID int64) error {
	if _, err := r.q.DeletePendingInvitationsByInviter(ctx, identitysqlc.DeletePendingInvitationsByInviterParams{
		TenantID: tenantID(ctx), InvitedBy: userID,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

// hydrate fills the derived role fields of a membership row written by a
// status mutation.
func (r *MembershipRepository) hydrate(ctx context.Context, row identitysqlc.Membership) (*identityservice.Membership, error) {
	role, roles, err := r.rolesOf(ctx, row.UserID)
	if err != nil {
		return nil, err
	}
	m := toMembership(row, role, roles)
	return &m, nil
}

func (r *MembershipRepository) rolesOf(ctx context.Context, userID int64) (role string, roles []string, err error) {
	member, err := r.Get(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	return member.Membership.Role, member.Membership.Roles, nil
}

func (r *MembershipRepository) insertRole(ctx context.Context, userID int64, role string) error {
	row, err := r.q.GetRoleByKey(ctx, identitysqlc.GetRoleByKeyParams{Key: role, TenantID: tenantID(ctx)})
	if err != nil {
		if isNotFound(wrapErr(err)) {
			return apperror.NewBadRequest("unknown role")
		}
		return wrapErr(err)
	}
	if err := r.q.InsertMembershipRole(ctx, identitysqlc.InsertMembershipRoleParams{
		TenantID: tenantID(ctx), UserID: userID, RoleID: row.ID,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *MembershipRepository) replaceRoles(ctx context.Context, userID int64, roles []string) error {
	tid := tenantID(ctx)
	if err := r.LockMembership(ctx, userID); err != nil {
		return err
	}
	if err := r.q.LockMembershipRoles(ctx, identitysqlc.LockMembershipRolesParams{
		TenantID: tid, UserID: userID,
	}); err != nil {
		return wrapErr(err)
	}
	roleIDs := make([]int64, 0, len(roles))
	for _, role := range roles {
		row, err := r.q.GetRoleByKey(ctx, identitysqlc.GetRoleByKeyParams{Key: role, TenantID: tid})
		if err != nil {
			if isNotFound(wrapErr(err)) {
				return apperror.NewBadRequest("unknown role")
			}
			return wrapErr(err)
		}
		roleIDs = append(roleIDs, row.ID)
	}
	if err := r.q.DeleteMemberRolesExcept(ctx, identitysqlc.DeleteMemberRolesExceptParams{
		TenantID: tid, UserID: userID, KeepRoleIds: roleIDs,
	}); err != nil {
		return wrapErr(err)
	}
	for _, roleID := range roleIDs {
		if err := r.q.InsertMembershipRole(ctx, identitysqlc.InsertMembershipRoleParams{
			TenantID: tid, UserID: userID, RoleID: roleID,
		}); err != nil {
			return wrapErr(err)
		}
	}
	return nil
}

func (r *MembershipRepository) CountActiveOwners(ctx context.Context) (int64, error) {
	n, err := r.q.CountActiveOwners(ctx, tenantID(ctx))
	if err != nil {
		return 0, wrapErr(err)
	}
	return n, nil
}

func (r *MembershipRepository) LockMembership(ctx context.Context, userID int64) error {
	n, err := r.q.LockMembership(ctx, identitysqlc.LockMembershipParams{
		TenantID: tenantID(ctx), UserID: userID,
	})
	if err != nil {
		return wrapErr(err)
	}
	if n == 0 {
		return apperror.ErrNotFound
	}
	return nil
}

func (r *MembershipRepository) LockOwnerChanges(ctx context.Context) error {
	if err := r.q.LockTenantOwnerChanges(ctx, tenantID(ctx)); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *MembershipRepository) Stats(ctx context.Context) (identityservice.MemberStats, error) {
	row, err := r.q.MembershipStats(ctx, tenantID(ctx))
	if err != nil {
		return identityservice.MemberStats{}, wrapErr(err)
	}
	return identityservice.MemberStats{Active: row.ActiveMembers, Inactive: row.InactiveMembers}, nil
}

func (r *MembershipRepository) EmailBanned(ctx context.Context, email string) (bool, error) {
	banned, err := r.q.EmailBannedInTenant(ctx, identitysqlc.EmailBannedInTenantParams{TenantID: tenantID(ctx), Email: email})
	if err != nil {
		return false, wrapErr(err)
	}
	return banned, nil
}

func (r *MembershipRepository) GetPrincipal(ctx context.Context, userID int64) (*identityservice.Principal, error) {
	row, err := r.q.GetMemberPrincipal(ctx, identitysqlc.GetMemberPrincipalParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPrincipal(row), nil
}

func (r *MembershipRepository) LockPrincipal(ctx context.Context, userID int64) (*identityservice.Principal, error) {
	row, err := r.q.LockMemberPrincipal(ctx, identitysqlc.LockMemberPrincipalParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPrincipal(identitysqlc.GetMemberPrincipalRow(row)), nil
}

func toPrincipal(row identitysqlc.GetMemberPrincipalRow) *identityservice.Principal {
	return &identityservice.Principal{
		UserID:               row.ID,
		TenantID:             row.TenantID,
		Email:                row.Email,
		Name:                 row.Name,
		Role:                 row.Role,
		Permissions:          row.Permissions,
		Platform:             row.Platform,
		TenantStatus:         identityservice.TenantStatus(row.TenantStatus),
		CredentialsRevokedAt: timePtr(row.CredentialsRevokedAt),
	}
}
