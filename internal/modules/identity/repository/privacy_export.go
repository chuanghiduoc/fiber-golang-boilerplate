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

func (r *PrivacyExportRepository) ListExternalIdentities(ctx context.Context, userID uuid.UUID, limit int32) ([]privacy.ExternalIdentityExport, bool, error) {
	if limit <= 0 {
		return []privacy.ExternalIdentityExport{}, false, nil
	}
	rows, err := r.q.ListUserExternalIdentities(ctx, identitysqlc.ListUserExternalIdentitiesParams{
		UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	rows, truncated := trimExportRows(rows, limit)
	out := make([]privacy.ExternalIdentityExport, len(rows))
	for i := range rows {
		out[i] = privacy.ExternalIdentityExport{
			Provider: rows[i].Provider,
			Subject:  rows[i].Subject,
			LinkedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, truncated, nil
}

func (r *PrivacyExportRepository) ListRoleAssignments(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]privacy.RoleAssignmentExport, bool, error) {
	if limit <= 0 {
		return []privacy.RoleAssignmentExport{}, false, nil
	}
	rows, err := r.q.ListUserRoleAssignments(ctx, identitysqlc.ListUserRoleAssignmentsParams{
		TenantID: tenantID, UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	rows, truncated := trimExportRows(rows, limit)
	out := make([]privacy.RoleAssignmentExport, len(rows))
	for i := range rows {
		out[i] = privacy.RoleAssignmentExport{Key: rows[i].Key, AssignedAt: rows[i].CreatedAt.Time}
	}
	return out, truncated, nil
}

func (r *PrivacyExportRepository) ListInvitationsSent(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]privacy.InvitationExport, bool, error) {
	if limit <= 0 {
		return []privacy.InvitationExport{}, false, nil
	}
	rows, err := r.q.ListUserInvitationsSent(ctx, identitysqlc.ListUserInvitationsSentParams{
		TenantID: tenantID, UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	rows, truncated := trimExportRows(rows, limit)
	out := make([]privacy.InvitationExport, len(rows))
	for i := range rows {
		out[i] = privacy.InvitationExport{
			ID: rows[i].ID, Email: rows[i].EmailNormalized, Role: rows[i].Role,
			InvitedBy: rows[i].InvitedBy, ExpiresAt: rows[i].ExpiresAt.Time,
			AcceptedAt: database.TimePtr(rows[i].AcceptedAt), AcceptedBy: rows[i].AcceptedBy,
			CreatedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, truncated, nil
}

func (r *PrivacyExportRepository) ListInvitationsReceived(ctx context.Context, tenantID, userID uuid.UUID, email string, limit int32) ([]privacy.InvitationExport, bool, error) {
	if limit <= 0 {
		return []privacy.InvitationExport{}, false, nil
	}
	rows, err := r.q.ListUserInvitationsReceived(ctx, identitysqlc.ListUserInvitationsReceivedParams{
		TenantID: tenantID, UserID: userID, Email: email, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	rows, truncated := trimExportRows(rows, limit)
	out := make([]privacy.InvitationExport, len(rows))
	for i := range rows {
		out[i] = privacy.InvitationExport{
			ID: rows[i].ID, Email: rows[i].EmailNormalized, Role: rows[i].Role,
			InvitedBy: rows[i].InvitedBy, ExpiresAt: rows[i].ExpiresAt.Time,
			AcceptedAt: database.TimePtr(rows[i].AcceptedAt), AcceptedBy: rows[i].AcceptedBy,
			CreatedAt: rows[i].CreatedAt.Time,
		}
	}
	return out, truncated, nil
}

func trimExportRows[T any](rows []T, limit int32) ([]T, bool) {
	if len(rows) > int(limit) {
		return rows[:limit], true
	}
	return rows, false
}
