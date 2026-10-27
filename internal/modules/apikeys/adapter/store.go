// Package adapter exposes the API-key module's persistence to the platform
// verifier without making platform code depend on module internals.
package adapter

import (
	"context"
	"time"

	"github.com/google/uuid"

	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
)

// VerifierStore adapts the key repository to apikey.Store.
type VerifierStore struct {
	repo apikeysvc.APIKeyRepository
}

var _ apikey.Store = (*VerifierStore)(nil)

func NewVerifierStore(repo apikeysvc.APIKeyRepository) *VerifierStore {
	return &VerifierStore{repo: repo}
}

func (s *VerifierStore) FindByHash(ctx context.Context, hash []byte) (apikey.Key, error) {
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
		ExpiresAt:          &key.ExpiresAt,
		RevokedAt:          key.RevokedAt,
		RotatedAt:          key.RotatedAt,
		RotationGraceUntil: key.RotationGraceUntil,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
	}, nil
}

func (s *VerifierStore) TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return s.repo.TouchLastUsed(ctx, id, at)
}
