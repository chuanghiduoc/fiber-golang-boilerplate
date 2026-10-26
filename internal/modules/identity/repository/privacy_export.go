package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

// PrivacyExportRepository implements the identity-only reads the GDPR export
// needs. audit_events stays behind platform/audit because identity does not
// own that table.
type PrivacyExportRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.IdentityExportRepository = (*PrivacyExportRepository)(nil)

func NewPrivacyExportRepository(db identitysqlc.DBTX) *PrivacyExportRepository {
	return &PrivacyExportRepository{q: identitysqlc.New(db)}
}

func (r *PrivacyExportRepository) ListExternalIdentities(ctx context.Context, userID int64) ([]identityservice.ExternalIdentityExport, error) {
	rows, err := r.q.ListUserExternalIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]identityservice.ExternalIdentityExport, len(rows))
	for i := range rows {
		out[i] = identityservice.ExternalIdentityExport{
			Provider: rows[i].Provider,
			Subject:  rows[i].Subject,
			LinkedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, nil
}

func (r *PrivacyExportRepository) ListRoleAssignments(ctx context.Context, tenantID, userID int64) ([]identityservice.RoleAssignmentExport, error) {
	rows, err := r.q.ListUserRoleAssignments(ctx, identitysqlc.ListUserRoleAssignmentsParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, err
	}
	out := make([]identityservice.RoleAssignmentExport, len(rows))
	for i := range rows {
		out[i] = identityservice.RoleAssignmentExport{Key: rows[i].Key, AssignedAt: rows[i].CreatedAt.Time}
	}
	return out, nil
}

func (r *PrivacyExportRepository) ListInvitationsSent(ctx context.Context, tenantID, userID int64) ([]identityservice.InvitationExport, error) {
	rows, err := r.q.ListUserInvitationsSent(ctx, identitysqlc.ListUserInvitationsSentParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, err
	}
	out := make([]identityservice.InvitationExport, len(rows))
	for i := range rows {
		out[i] = identityservice.InvitationExport{
			ID: rows[i].ID, Email: rows[i].EmailNormalized, Role: rows[i].Role,
			InvitedBy: rows[i].InvitedBy, ExpiresAt: rows[i].ExpiresAt.Time,
			AcceptedAt: timePtr(rows[i].AcceptedAt), AcceptedBy: pgInt8Ptr(rows[i].AcceptedBy),
			CreatedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, nil
}

func (r *PrivacyExportRepository) ListInvitationsReceived(ctx context.Context, tenantID, userID int64, email string) ([]identityservice.InvitationExport, error) {
	rows, err := r.q.ListUserInvitationsReceived(ctx, identitysqlc.ListUserInvitationsReceivedParams{
		TenantID: tenantID, UserID: userID, Email: email,
	})
	if err != nil {
		return nil, err
	}
	out := make([]identityservice.InvitationExport, len(rows))
	for i := range rows {
		out[i] = identityservice.InvitationExport{
			ID: rows[i].ID, Email: rows[i].EmailNormalized, Role: rows[i].Role,
			InvitedBy: rows[i].InvitedBy, ExpiresAt: rows[i].ExpiresAt.Time,
			AcceptedAt: timePtr(rows[i].AcceptedAt), AcceptedBy: pgInt8Ptr(rows[i].AcceptedBy),
			CreatedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, nil
}

func pgInt8Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
