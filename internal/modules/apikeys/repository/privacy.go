package repository

import (
	"context"
	"time"

	apikeysprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/privacy"
	apikeyssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/sqlc"
)

// PrivacyStore implements apikeys/privacy.Store over sqlc.
type PrivacyStore struct {
	q *apikeyssqlc.Queries
}

var _ apikeysprivacy.Store = (*PrivacyStore)(nil)

func NewPrivacyStore(db apikeyssqlc.DBTX) *PrivacyStore {
	return &PrivacyStore{q: apikeyssqlc.New(db)}
}

func (s *PrivacyStore) List(ctx context.Context, tenantID, userID int64) ([]apikeysprivacy.Key, error) {
	rows, err := s.q.ListAPIKeysForPrivacy(ctx, apikeyssqlc.ListAPIKeysForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]apikeysprivacy.Key, len(rows))
	for i, row := range rows {
		out[i] = apikeysprivacy.Key{
			ID: row.ID, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
			ExpiresAt:          privacyTimePtr(row.ExpiresAt.Time, row.ExpiresAt.Valid),
			RevokedAt:          privacyTimePtr(row.RevokedAt.Time, row.RevokedAt.Valid),
			RotatedAt:          privacyTimePtr(row.RotatedAt.Time, row.RotatedAt.Valid),
			RotationGraceUntil: privacyTimePtr(row.RotationGraceUntil.Time, row.RotationGraceUntil.Valid),
			LastUsedAt:         privacyTimePtr(row.LastUsedAt.Time, row.LastUsedAt.Valid),
			CreatedAt:          row.CreatedAt.Time,
			UpdatedAt:          row.UpdatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) DeleteAll(ctx context.Context, tenantID, userID int64) error {
	_, err := s.q.DeleteAPIKeysForPrivacy(ctx, apikeyssqlc.DeleteAPIKeysForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
	return err
}

func privacyTimePtr(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}
