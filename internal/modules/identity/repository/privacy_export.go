package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/privacy"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/google/uuid"
)

// PrivacyExportRepository implements the identity-only reads the GDPR export
// needs. audit_events stays behind platform/audit because identity does not
// own that table.
type PrivacyExportRepository struct {
	q *identitysqlc.Queries
}

var _ privacy.IdentityExportRepository = (*PrivacyExportRepository)(nil)

func NewPrivacyExportRepository(db identitysqlc.DBTX) *PrivacyExportRepository {
	return &PrivacyExportRepository{q: identitysqlc.New(db)}
}

func (r *PrivacyExportRepository) ListExternalIdentities(ctx context.Context, userID uuid.UUID) ([]privacy.ExternalIdentityExport, error) {
	rows, err := r.q.ListUserExternalIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]privacy.ExternalIdentityExport, len(rows))
	for i := range rows {
		out[i] = privacy.ExternalIdentityExport{
			Provider: rows[i].Provider,
			Subject:  rows[i].Subject,
			LinkedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, nil
}

func (r *PrivacyExportRepository) ListRoleAssignments(ctx context.Context, tenantID, userID uuid.UUID) ([]privacy.RoleAssignmentExport, error) {
	rows, err := r.q.ListUserRoleAssignments(ctx, identitysqlc.ListUserRoleAssignmentsParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, err
	}
	out := make([]privacy.RoleAssignmentExport, len(rows))
	for i := range rows {
		out[i] = privacy.RoleAssignmentExport{Key: rows[i].Key, AssignedAt: rows[i].CreatedAt.Time}
	}
	return out, nil
}

func (r *PrivacyExportRepository) ListInvitationsSent(ctx context.Context, tenantID, userID uuid.UUID) ([]privacy.InvitationExport, error) {
	rows, err := r.q.ListUserInvitationsSent(ctx, identitysqlc.ListUserInvitationsSentParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		return nil, err
	}
	out := make([]privacy.InvitationExport, len(rows))
	for i := range rows {
		out[i] = privacy.InvitationExport{
			ID: rows[i].ID, Email: rows[i].EmailNormalized, Role: rows[i].Role,
			InvitedBy: rows[i].InvitedBy, ExpiresAt: rows[i].ExpiresAt.Time,
			AcceptedAt: database.TimePtr(rows[i].AcceptedAt), AcceptedBy: rows[i].AcceptedBy,
			CreatedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, nil
}

func (r *PrivacyExportRepository) ListInvitationsReceived(ctx context.Context, tenantID, userID uuid.UUID, email string) ([]privacy.InvitationExport, error) {
	rows, err := r.q.ListUserInvitationsReceived(ctx, identitysqlc.ListUserInvitationsReceivedParams{
		TenantID: tenantID, UserID: userID, Email: email,
	})
	if err != nil {
		return nil, err
	}
	out := make([]privacy.InvitationExport, len(rows))
	for i := range rows {
		out[i] = privacy.InvitationExport{
			ID: rows[i].ID, Email: rows[i].EmailNormalized, Role: rows[i].Role,
			InvitedBy: rows[i].InvitedBy, ExpiresAt: rows[i].ExpiresAt.Time,
			AcceptedAt: database.TimePtr(rows[i].AcceptedAt), AcceptedBy: rows[i].AcceptedBy,
			CreatedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, nil
}
