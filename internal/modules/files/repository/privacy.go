package repository

import (
	"context"

	"github.com/google/uuid"

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

// ListForUserLimited returns one bounded page for an export and reports
// whether more rows exist. The query fetches limit+1 rows so an exact-limit
// result is not mistaken for truncation.
func (r *PrivacyRepository) ListForUserLimited(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]filesvc.File, bool, error) {
	if limit <= 0 {
		return []filesvc.File{}, false, nil
	}
	fetchLimit := limit + 1
	rows, err := r.q.ListFilesForPrivacyLimited(ctx, filesqlc.ListFilesForPrivacyLimitedParams{
		TenantID: tenantID,
		UserID:   userID,
		RowLimit: fetchLimit,
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > int(limit)
	if truncated {
		rows = rows[:limit]
	}
	files := make([]filesvc.File, len(rows))
	for i := range rows {
		files[i] = *toFile(rows[i])
	}
	return files, truncated, nil
}

func (r *PrivacyRepository) HardDelete(ctx context.Context, tenantID, userID, id uuid.UUID) error {
	_, err := r.q.DeleteFileForPrivacy(ctx, filesqlc.DeleteFileForPrivacyParams{
		ID:       id,
		TenantID: tenantID,
		UserID:   userID,
	})
	return err
}
