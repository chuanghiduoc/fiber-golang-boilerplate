// Package adapter exposes the API-key module's persistence to the platform
// verifier without making platform code depend on module internals.
package adapter

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
)

// VerifierStore adapts APIKeyRepository to apikey.Store.
type VerifierStore struct {
	repo repository.APIKeyRepository
}

var _ apikey.Store = (*VerifierStore)(nil)

func NewVerifierStore(repo repository.APIKeyRepository) *VerifierStore {
	return &VerifierStore{repo: repo}
}

func (s *VerifierStore) FindByHash(ctx context.Context, hash string) (apikey.Key, error) {
	key, err := s.repo.GetByHash(ctx, hash)
	if err != nil {
		return apikey.Key{}, err
	}
	return apikey.Key{
		ID:                 key.ID,
		TenantID:           key.TenantID,
		UserID:             key.UserID,
		Name:               key.Name,
		Prefix:             key.Prefix,
		Hash:               key.Hash,
		Scopes:             key.Scopes,
		ExpiresAt:          key.ExpiresAt,
		RevokedAt:          key.RevokedAt,
		RotatedAt:          key.RotatedAt,
		RotationGraceUntil: key.RotationGraceUntil,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
	}, nil
}

func (s *VerifierStore) TouchLastUsed(ctx context.Context, id int64, at time.Time) error {
	return s.repo.TouchLastUsed(ctx, id, at)
}
