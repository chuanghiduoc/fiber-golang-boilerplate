package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
)

// InvitationRepository implements core.InvitationRepository.
type InvitationRepository struct {
	q *identitysqlc.Queries
}

var _ core.InvitationRepository = (*InvitationRepository)(nil)

func NewInvitationRepository(db identitysqlc.DBTX) *InvitationRepository {
	return &InvitationRepository{q: identitysqlc.New(db)}
}

func toInvitation(row identitysqlc.Invitation, role string) core.Invitation {
	return core.Invitation{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Email:      row.EmailNormalized,
		Role:       role,
		InvitedBy:  row.InvitedBy,
		ExpiresAt:  row.ExpiresAt.Time,
		AcceptedAt: database.TimePtr(row.AcceptedAt),
		AcceptedBy: row.AcceptedBy,
		CreatedAt:  row.CreatedAt.Time,
		UpdatedAt:  row.UpdatedAt.Time,
	}
}

func (r *InvitationRepository) Upsert(ctx context.Context, invitation core.NewInvitation) (*core.Invitation, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	role, err := r.q.GetRoleByKey(ctx, identitysqlc.GetRoleByKeyParams{Key: invitation.Role, TenantID: tenantID})
	if err != nil {
		if isNotFound(wrapErr(err)) {
			return nil, apperror.NewBadRequest("unknown role")
		}
		return nil, wrapErr(err)
	}
	row, err := r.q.UpsertInvitation(ctx, identitysqlc.UpsertInvitationParams{
		TenantID:        tenantID,
		EmailNormalized: invitation.Email,
		RoleID:          role.ID,
		TokenHash:       invitation.TokenHash,
		InvitedBy:       invitation.InvitedBy,
		ExpiresAt:       database.Timestamptz(invitation.ExpiresAt),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	inv := toInvitation(row, invitation.Role)
	return &inv, nil
}

func (r *InvitationRepository) Get(ctx context.Context, id uuid.UUID) (*core.Invitation, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetInvitation(ctx, identitysqlc.GetInvitationParams{TenantID: tenantID, ID: id})
	if err != nil {
		return nil, wrapErr(err)
	}
	inv := toInvitation(row.Invitation, row.Role)
	return &inv, nil
}

func (r *InvitationRepository) ListPendingCursor(ctx context.Context, cur pagination.DBCursor) ([]core.Invitation, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListPendingInvitationsCursor(ctx, identitysqlc.ListPendingInvitationsCursorParams{
		TenantID:        tenantID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	out := make([]core.Invitation, len(rows))
	for i := range rows {
		out[i] = toInvitation(rows[i].Invitation, rows[i].Role)
	}
	return out, nil
}

func (r *InvitationRepository) DeletePending(ctx context.Context, id uuid.UUID) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := r.q.DeletePendingInvitation(ctx, identitysqlc.DeletePendingInvitationParams{TenantID: tenantID, ID: id})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *InvitationRepository) TenantByTokenHash(ctx context.Context, tokenHash []byte) (uuid.UUID, error) {
	tenantID, err := r.q.GetInvitationTenantByTokenHash(ctx, tokenHash)
	if err != nil {
		return uuid.Nil, wrapErr(err)
	}
	return tenantID, nil
}

func (r *InvitationRepository) GetByTokenHashForUpdate(ctx context.Context, tenantID uuid.UUID, tokenHash []byte) (*core.Invitation, error) {
	row, err := r.q.GetInvitationByTokenHashForUpdate(ctx, identitysqlc.GetInvitationByTokenHashForUpdateParams{
		TenantID:  tenantID,
		TokenHash: tokenHash,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	inv := toInvitation(row.Invitation, row.Role)
	return &inv, nil
}

func (r *InvitationRepository) MarkAccepted(ctx context.Context, scopedTenantID, id, userID uuid.UUID) (bool, error) {
	n, err := r.q.MarkInvitationAccepted(ctx, identitysqlc.MarkInvitationAcceptedParams{
		TenantID:   scopedTenantID,
		ID:         id,
		AcceptedBy: uuid.NullUUID{UUID: userID, Valid: true},
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}
