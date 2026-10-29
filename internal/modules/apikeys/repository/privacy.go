package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

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

func (s *PrivacyStore) List(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]apikeysprivacy.Key, bool, error) {
	if limit <= 0 {
		return []apikeysprivacy.Key{}, false, nil
	}
	rows, err := s.q.ListAPIKeysForPrivacy(ctx, apikeyssqlc.ListAPIKeysForPrivacyParams{
		TenantID: tenantID, UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > int(limit)
	if truncated {
		rows = rows[:limit]
	}
	out := make([]apikeysprivacy.Key, len(rows))
	for i, row := range rows {
		out[i] = apikeysprivacy.Key{
			ID: row.ID, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
			ExpiresAt:          row.ExpiresAt.Time,
			RevokedAt:          privacyTimePtr(row.RevokedAt.Time, row.RevokedAt.Valid),
			RotatedAt:          privacyTimePtr(row.RotatedAt.Time, row.RotatedAt.Valid),
			RotationGraceUntil: privacyTimePtr(row.RotationGraceUntil.Time, row.RotationGraceUntil.Valid),
			LastUsedAt:         privacyTimePtr(row.LastUsedAt.Time, row.LastUsedAt.Valid),
			CreatedAt:          row.CreatedAt.Time,
			UpdatedAt:          row.UpdatedAt.Time,
		}
	}
	return out, truncated, nil
}

func (s *PrivacyStore) DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error {
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
