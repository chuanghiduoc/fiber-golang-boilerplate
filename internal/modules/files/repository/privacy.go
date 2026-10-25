package repository

import (
	"context"

	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
)

// PrivacyRepository implements service.PrivacyRepository. The tenant and user
// are explicit because the privacy contract passes them, not a request context.
type PrivacyRepository struct {
	q *filesqlc.Queries
}

var _ filesvc.PrivacyRepository = (*PrivacyRepository)(nil)

func NewPrivacyRepository(db filesqlc.DBTX) *PrivacyRepository {
	return &PrivacyRepository{q: filesqlc.New(db)}
}

func (r *PrivacyRepository) ListForUser(ctx context.Context, tenantID, userID int64) ([]filesvc.File, error) {
	rows, err := r.q.ListFilesForPrivacy(ctx, filesqlc.ListFilesForPrivacyParams{
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		return nil, err
	}
	files := make([]filesvc.File, len(rows))
	for i := range rows {
		files[i] = *toFile(rows[i])
	}
	return files, nil
}

func (r *PrivacyRepository) HardDelete(ctx context.Context, tenantID, userID, id int64) error {
	_, err := r.q.DeleteFileForPrivacy(ctx, filesqlc.DeleteFileForPrivacyParams{
		ID:       id,
		TenantID: tenantID,
		UserID:   userID,
	})
	return err
}
