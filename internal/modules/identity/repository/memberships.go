package repository

import (
	"context"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
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

func toMembership(row identitysqlc.Membership) identityservice.Membership {
	return identityservice.Membership{
		ID:                   row.ID,
		TenantID:             row.TenantID,
		UserID:               row.UserID,
		Role:                 row.Role,
		Status:               identityservice.MembershipStatus(row.Status),
		BannedAt:             timePtr(row.BannedAt),
		RemovedAt:            timePtr(row.RemovedAt),
		CredentialsRevokedAt: timePtr(row.CredentialsRevokedAt),
		CreatedAt:            row.CreatedAt.Time,
		UpdatedAt:            row.UpdatedAt.Time,
	}
}

func toMember(user identitysqlc.User, membership identitysqlc.Membership, hasPassword bool) identityservice.Member {
	return identityservice.Member{
		User:       *toUser(user, hasPassword),
		Membership: toMembership(membership),
	}
}

func oneMembership(row identitysqlc.Membership, err error) (*identityservice.Membership, error) {
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMembership(row)
	return &m, nil
}

func (r *MembershipRepository) Get(ctx context.Context, userID int64) (*identityservice.Member, error) {
	row, err := r.q.GetMember(ctx, identitysqlc.GetMemberParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMember(row.User, row.Membership, row.HasPassword)
	return &m, nil
}

func (r *MembershipRepository) GetActive(ctx context.Context, userID int64) (*identityservice.Member, error) {
	row, err := r.q.GetActiveMember(ctx, identitysqlc.GetActiveMemberParams{TenantID: tenantID(ctx), UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	m := toMember(row.User, row.Membership, row.HasPassword)
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
		members[i] = toMember(rows[i].User, rows[i].Membership, rows[i].HasPassword)
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
		members[i] = toMember(rows[i].User, rows[i].Membership, rows[i].HasPassword)
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
		memberships[i] = toMembership(rows[i])
	}
	return memberships, nil
}

func (r *MembershipRepository) Create(ctx context.Context, userID int64, role string) (*identityservice.Membership, error) {
	row, err := r.q.CreateMembership(ctx, identitysqlc.CreateMembershipParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
		Role:     role,
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == uniqueMembershipsTenantUser {
			return nil, identityservice.ErrAlreadyMember
		}
		return nil, wrapErr(err)
	}
	m := toMembership(row)
	return &m, nil
}

func (r *MembershipRepository) Reactivate(ctx context.Context, userID int64, role string) (*identityservice.Membership, error) {
	return oneMembership(r.q.ReactivateMembership(ctx, identitysqlc.ReactivateMembershipParams{
		TenantID: tenantID(ctx), UserID: userID, Role: role,
	}))
}

func (r *MembershipRepository) UpdateRole(ctx context.Context, userID int64, role string) (*identityservice.Membership, error) {
	return oneMembership(r.q.UpdateMembershipRole(ctx, identitysqlc.UpdateMembershipRoleParams{
		TenantID: tenantID(ctx), UserID: userID, Role: role,
	}))
}

func (r *MembershipRepository) Ban(ctx context.Context, userID int64) (*identityservice.Membership, error) {
	return oneMembership(r.q.BanMembership(ctx, identitysqlc.BanMembershipParams{TenantID: tenantID(ctx), UserID: userID}))
}

func (r *MembershipRepository) Unban(ctx context.Context, userID int64) (*identityservice.Membership, error) {
	return oneMembership(r.q.UnbanMembership(ctx, identitysqlc.UnbanMembershipParams{TenantID: tenantID(ctx), UserID: userID}))
}

func (r *MembershipRepository) Remove(ctx context.Context, userID int64) (*identityservice.Membership, error) {
	return oneMembership(r.q.RemoveMembership(ctx, identitysqlc.RemoveMembershipParams{TenantID: tenantID(ctx), UserID: userID}))
}

func (r *MembershipRepository) CountActiveAdmins(ctx context.Context) (int64, error) {
	n, err := r.q.CountActiveAdmins(ctx, tenantID(ctx))
	if err != nil {
		return 0, wrapErr(err)
	}
	return n, nil
}

func (r *MembershipRepository) LockAdminChanges(ctx context.Context) error {
	if err := r.q.LockTenantAdminChanges(ctx, tenantID(ctx)); err != nil {
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
	// Both queries select the same columns, so the generated rows convert.
	return toPrincipal(identitysqlc.GetMemberPrincipalRow(row)), nil
}

func toPrincipal(row identitysqlc.GetMemberPrincipalRow) *identityservice.Principal {
	return &identityservice.Principal{
		UserID:               row.ID,
		TenantID:             row.TenantID,
		Email:                row.Email,
		Name:                 row.Name,
		Role:                 row.Role,
		TenantStatus:         identityservice.TenantStatus(row.TenantStatus),
		CredentialsRevokedAt: timePtr(row.CredentialsRevokedAt),
	}
}
