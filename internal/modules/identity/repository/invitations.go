package repository

import (
	"context"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// InvitationRepository implements service.InvitationRepository.
type InvitationRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.InvitationRepository = (*InvitationRepository)(nil)

func NewInvitationRepository(db identitysqlc.DBTX) *InvitationRepository {
	return &InvitationRepository{q: identitysqlc.New(db)}
}

func toInvitation(row identitysqlc.Invitation) identityservice.Invitation {
	inv := identityservice.Invitation{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Email:      row.EmailNormalized,
		Role:       row.Role,
		InvitedBy:  row.InvitedBy,
		ExpiresAt:  row.ExpiresAt.Time,
		AcceptedAt: timePtr(row.AcceptedAt),
		CreatedAt:  row.CreatedAt.Time,
		UpdatedAt:  row.UpdatedAt.Time,
	}
	if row.AcceptedBy.Valid {
		by := row.AcceptedBy.Int64
		inv.AcceptedBy = &by
	}
	return inv
}

func (r *InvitationRepository) Upsert(ctx context.Context, invitation identityservice.NewInvitation) (*identityservice.Invitation, error) {
	row, err := r.q.UpsertInvitation(ctx, identitysqlc.UpsertInvitationParams{
		TenantID:        tenantID(ctx),
		EmailNormalized: invitation.Email,
		Role:            invitation.Role,
		TokenHash:       invitation.TokenHash,
		InvitedBy:       invitation.InvitedBy,
		ExpiresAt:       timestamptz(invitation.ExpiresAt),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	inv := toInvitation(row)
	return &inv, nil
}

func (r *InvitationRepository) ListPendingCursor(ctx context.Context, cur pagination.DBCursor) ([]identityservice.Invitation, error) {
	rows, err := r.q.ListPendingInvitationsCursor(ctx, identitysqlc.ListPendingInvitationsCursorParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	out := make([]identityservice.Invitation, len(rows))
	for i := range rows {
		out[i] = toInvitation(rows[i])
	}
	return out, nil
}

func (r *InvitationRepository) DeletePending(ctx context.Context, id int64) (bool, error) {
	n, err := r.q.DeletePendingInvitation(ctx, identitysqlc.DeletePendingInvitationParams{TenantID: tenantID(ctx), ID: id})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *InvitationRepository) TenantByTokenHash(ctx context.Context, tokenHash string) (int64, error) {
	tenant, err := r.q.GetInvitationTenantByTokenHash(ctx, tokenHash)
	if err != nil {
		return 0, wrapErr(err)
	}
	return tenant, nil
}

func (r *InvitationRepository) GetByTokenHashForUpdate(ctx context.Context, tenantID int64, tokenHash string) (*identityservice.Invitation, error) {
	row, err := r.q.GetInvitationByTokenHashForUpdate(ctx, identitysqlc.GetInvitationByTokenHashForUpdateParams{
		TenantID:  tenantID,
		TokenHash: tokenHash,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	inv := toInvitation(row)
	return &inv, nil
}

func (r *InvitationRepository) MarkAccepted(ctx context.Context, tenant, id, userID int64) (bool, error) {
	n, err := r.q.MarkInvitationAccepted(ctx, identitysqlc.MarkInvitationAcceptedParams{
		TenantID:   tenant,
		ID:         id,
		AcceptedBy: pgInt8(userID),
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}
