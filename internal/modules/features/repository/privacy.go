package repository

import (
	"context"

	"github.com/google/uuid"

	featuresprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/privacy"
	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
)

// PrivacyStore implements features/privacy.Store over sqlc.
type PrivacyStore struct {
	q *featuressqlc.Queries
}

var _ featuresprivacy.Store = (*PrivacyStore)(nil)

func NewPrivacyStore(db featuressqlc.DBTX) *PrivacyStore {
	return &PrivacyStore{q: featuressqlc.New(db)}
}

func (s *PrivacyStore) List(ctx context.Context, tenantID, userID uuid.UUID) ([]featuresprivacy.Target, error) {
	rows, err := s.q.ListFeatureFlagTargetsForPrivacy(ctx, featuressqlc.ListFeatureFlagTargetsForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]featuresprivacy.Target, len(rows))
	for i, row := range rows {
		out[i] = featuresprivacy.Target{
			FlagKey: row.FlagKey, Enabled: row.Enabled,
			CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error {
	_, err := s.q.DeleteFeatureFlagTargetsForPrivacy(ctx, featuressqlc.DeleteFeatureFlagTargetsForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
	return err
}
