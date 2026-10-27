package repository

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
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
		BannedAt:             database.TimePtr(row.BannedAt),
		BannedEmailHash:      row.BannedEmailHash,
		ErasureRequestedAt:   database.TimePtr(row.ErasureRequestedAt),
		RemovedAt:            database.TimePtr(row.RemovedAt),
		CredentialsRevokedAt: database.TimePtr(row.CredentialsRevokedAt),
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

func (r *MembershipRepository) Get(ctx context.Context, userID uuid.UUID) (*identityservice.Member, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetMember(ctx, identitysqlc.GetMemberParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMember(row.User, row.Membership, row.Role, row.Roles, row.HasPassword)
	return &m, nil
}

func (r *MembershipRepository) GetActive(ctx context.Context, userID uuid.UUID) (*identityservice.Member, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetActiveMember(ctx, identitysqlc.GetActiveMemberParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMember(row.User, row.Membership, row.Role, row.Roles, row.HasPassword)
	return &m, nil
}

func (r *MembershipRepository) ListActiveCursor(ctx context.Context, cur pagination.DBCursor) ([]identityservice.Member, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListActiveMembersCursor(ctx, identitysqlc.ListActiveMembersCursorParams{
		TenantID:        tenantID,
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
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListMembersCursor(ctx, identitysqlc.ListMembersCursorParams{
		TenantID:        tenantID,
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

func (r *MembershipRepository) ListFilteredCursor(ctx context.Context, cur pagination.DBCursor, status, role string) ([]identityservice.Member, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	params := identitysqlc.ListMembersFilteredCursorParams{
		TenantID:        tenantID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	}
	if status != "" {
		params.Status = pgtype.Text{String: status, Valid: true}
	}
	if role != "" {
		params.Role = pgtype.Text{String: role, Valid: true}
	}
	rows, err := r.q.ListMembersFilteredCursor(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	members := make([]identityservice.Member, len(rows))
	for i := range rows {
		members[i] = toMember(rows[i].User, rows[i].Membership, rows[i].Role, rows[i].Roles, rows[i].HasPassword)
	}
	return members, nil
}

func (r *MembershipRepository) ListForUser(ctx context.Context, userID uuid.UUID) ([]identityservice.Membership, error) {
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

func (r *MembershipRepository) Create(ctx context.Context, userID uuid.UUID, role string) (*identityservice.Membership, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.CreateMembership(ctx, identitysqlc.CreateMembershipParams{
		TenantID: tenantID,
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

func (r *MembershipRepository) Reactivate(ctx context.Context, userID uuid.UUID, role string) (*identityservice.Membership, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.ReactivateMembership(ctx, identitysqlc.ReactivateMembershipParams{
		TenantID: tenantID, UserID: userID,
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

func (r *MembershipRepository) UpdateRole(ctx context.Context, userID uuid.UUID, role string) (*identityservice.Membership, error) {
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

func (r *MembershipRepository) Ban(ctx context.Context, userID uuid.UUID, marker identityservice.BanMarker) (*identityservice.Membership, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.BanMembership(ctx, identitysqlc.BanMembershipParams{
		TenantID:           tenantID,
		UserID:             userID,
		BannedEmailHash:    marker.Hash,
		BannedEmailHashKid: marker.KeyID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	if err := r.deleteInvitationsCreatedBy(ctx, userID); err != nil {
		return nil, err
	}
	return r.hydrate(ctx, row)
}

func (r *MembershipRepository) Unban(ctx context.Context, userID uuid.UUID) (*identityservice.Membership, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.UnbanMembership(ctx, identitysqlc.UnbanMembershipParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return r.hydrate(ctx, row)
}

func (r *MembershipRepository) Remove(ctx context.Context, userID uuid.UUID, marker identityservice.BanMarker) (*identityservice.Membership, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.RemoveMembership(ctx, identitysqlc.RemoveMembershipParams{
		TenantID:           tenantID,
		UserID:             userID,
		BannedEmailHash:    marker.Hash,
		BannedEmailHashKid: marker.KeyID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	// Removed memberships lose their roles, so a tenant role they held can be
	// deleted without waiting for a re-invitation.
	if err := r.q.DeleteMemberRoles(ctx, identitysqlc.DeleteMemberRolesParams{
		TenantID: tenantID, UserID: userID,
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
func (r *MembershipRepository) SuspendForErasure(ctx context.Context, userID uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	rows, err := r.q.SuspendMembershipForErasure(ctx, identitysqlc.SuspendMembershipForErasureParams{
		TenantID: tenantID, UserID: userID,
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
func (r *MembershipRepository) deleteInvitationsCreatedBy(ctx context.Context, userID uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	if _, err := r.q.DeletePendingInvitationsByInviter(ctx, identitysqlc.DeletePendingInvitationsByInviterParams{
		TenantID: tenantID, InvitedBy: userID,
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

func (r *MembershipRepository) rolesOf(ctx context.Context, userID uuid.UUID) (role string, roles []string, err error) {
	member, err := r.Get(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	return member.Membership.Role, member.Membership.Roles, nil
}

func (r *MembershipRepository) insertRole(ctx context.Context, userID uuid.UUID, role string) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	row, err := r.q.GetRoleByKey(ctx, identitysqlc.GetRoleByKeyParams{Key: role, TenantID: tenantID})
	if err != nil {
		if isNotFound(wrapErr(err)) {
			return apperror.NewBadRequest("unknown role")
		}
		return wrapErr(err)
	}
	if err := r.q.InsertMembershipRole(ctx, identitysqlc.InsertMembershipRoleParams{
		TenantID: tenantID, UserID: userID, RoleID: row.ID,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *MembershipRepository) replaceRoles(ctx context.Context, userID uuid.UUID, roles []string) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tid := tenantID
	if err := r.LockMembership(ctx, userID); err != nil {
		return err
	}
	if err := r.q.LockMembershipRoles(ctx, identitysqlc.LockMembershipRolesParams{
		TenantID: tid, UserID: userID,
	}); err != nil {
		return wrapErr(err)
	}
	roleIDs := make([]uuid.UUID, 0, len(roles))
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
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return 0, err
	}
	n, err := r.q.CountActiveOwners(ctx, tenantID)
	if err != nil {
		return 0, wrapErr(err)
	}
	return n, nil
}

func (r *MembershipRepository) LockMembership(ctx context.Context, userID uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	n, err := r.q.LockMembership(ctx, identitysqlc.LockMembershipParams{
		TenantID: tenantID, UserID: userID,
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
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	if err := r.q.LockTenantOwnerChanges(ctx, tenantID); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *MembershipRepository) Stats(ctx context.Context) (identityservice.MemberStats, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return identityservice.MemberStats{}, err
	}
	row, err := r.q.MembershipStats(ctx, tenantID)
	if err != nil {
		return identityservice.MemberStats{}, wrapErr(err)
	}
	return identityservice.MemberStats{Active: row.ActiveMembers, Inactive: row.InactiveMembers}, nil
}

func (r *MembershipRepository) EmailBanned(ctx context.Context, hashes [][]byte) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	banned, err := r.q.EmailBannedInTenant(ctx, identitysqlc.EmailBannedInTenantParams{
		TenantID: tenantID, EmailHashes: hashes,
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return banned, nil
}

// BannedMarkerCounts counts retained ban markers per key id across every
// tenant. It is the startup check for markers written by a key that is no
// longer configured; those bans can no longer match an address.
func (r *MembershipRepository) BannedMarkerCounts(ctx context.Context) ([]identityservice.BanMarkerCount, error) {
	ctx = database.WithRLSBypass(ctx) // Counts every tenant's retained bans.
	rows, err := r.q.CountBannedMarkers(ctx)
	if err != nil {
		return nil, wrapErr(err)
	}
	counts := make([]identityservice.BanMarkerCount, len(rows))
	for i, row := range rows {
		counts[i] = identityservice.BanMarkerCount{KeyID: row.BannedEmailHashKid, Bans: row.Bans}
	}
	return counts, nil
}

func (r *MembershipRepository) GetPrincipal(ctx context.Context, userID uuid.UUID) (*identityservice.Principal, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetMemberPrincipal(ctx, identitysqlc.GetMemberPrincipalParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPrincipal(row), nil
}

func (r *MembershipRepository) LockPrincipal(ctx context.Context, userID uuid.UUID) (*identityservice.Principal, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.LockMemberPrincipal(ctx, identitysqlc.LockMemberPrincipalParams{TenantID: tenantID, UserID: userID})
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
		CredentialsRevokedAt: database.TimePtr(row.CredentialsRevokedAt),
	}
}

// TouchIfUnchanged is the optimistic-concurrency gate for the memberships
// ETag. It returns false when another writer already changed the row.
func (r *MembershipRepository) TouchIfUnchanged(ctx context.Context, userID uuid.UUID, expected time.Time) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := r.q.TouchMembershipIfUnchanged(ctx, identitysqlc.TouchMembershipIfUnchangedParams{
		TenantID:          tenantID,
		UserID:            userID,
		ExpectedUpdatedAt: timestamptzValue(expected),
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}
