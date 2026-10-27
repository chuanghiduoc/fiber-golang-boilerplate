package apikey

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// Key is the verifier's view of a stored API key. Hash is included for
// completeness but is never exposed outside the platform/module boundary.
type Key struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	UserID             uuid.UUID
	Name               string
	Prefix             string
	Hash               []byte
	Scopes             []authz.Permission
	ExpiresAt          *time.Time
	RevokedAt          *time.Time
	RotatedAt          *time.Time
	RotationGraceUntil *time.Time
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Store is the narrow persistence contract the verifier needs. The API-key
// module implements it; platform code never imports module internals.
type Store interface {
	// FindByHash returns the key with the supplied SHA-256 hash. Implementations
	// must return apperror.ErrNotFound (or a wrapped error) when no row exists.
	FindByHash(ctx context.Context, hash []byte) (Key, error)
	// TouchLastUsed records a successful authentication. Implementations should
	// treat a missing row as a no-op.
	TouchLastUsed(ctx context.Context, id uuid.UUID, at time.Time) error
}
