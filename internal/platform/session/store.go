// Package session validates access-token claims against the current user
// record so bans, role changes, and password changes take effect immediately
// instead of waiting for the JWT to expire.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

const (
	cacheTTL      = 5 * time.Second
	generationTTL = 24 * time.Hour
)

// Store validates a token against the user's current role/token version and
// tenant. Suspended tenants and tokens whose tenant claim does not match the
// stored user row are rejected.
type Store interface {
	Validate(ctx context.Context, userID, tenantID int64, role string, tokenVersion int32) error
	Invalidate(ctx context.Context, userID int64) error
}

type snapshot struct {
	Role         string `json:"role"`
	TokenVersion int32  `json:"tokenVersion"`
	TenantID     int64  `json:"tenantId"`
	TenantStatus string `json:"tenantStatus"`
}

// userStore is the narrow read contract the session validator needs. Keeping
// it local prevents platform code from depending on a business module or its
// generated SQL types.
type userStore interface {
	GetSessionState(ctx context.Context, id int64) (role string, tokenVersion int32, tenantID int64, tenantStatus string, err error)
}

type store struct {
	users userStore
	cache cache.Cache
}

// NewStore creates a session validator. A cache is optional; without one every
// authenticated request performs a single indexed user lookup.
func NewStore(users userStore, appCache cache.Cache) Store {
	return &store{users: users, cache: appCache}
}

func generationKey(userID int64) string {
	return fmt.Sprintf("session:generation:%d", userID)
}

func snapshotKey(userID, generation int64) string {
	return fmt.Sprintf("session:user:%d:%d", userID, generation)
}

// currentGeneration reads a monotonically increasing per-user generation. A
// snapshot is stored under the generation it was read at, so a slow request
// that finishes after an invalidation writes to an obsolete key instead of
// resurrecting a stale role/token version.
func (s *store) currentGeneration(ctx context.Context, userID int64) int64 {
	if s.cache == nil {
		return 0
	}
	data, err := s.cache.Get(ctx, generationKey(userID))
	if err != nil || data == nil {
		return 0
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *store) Validate(ctx context.Context, userID, tenantID int64, role string, tokenVersion int32) error {
	generation := s.currentGeneration(ctx, userID)
	key := snapshotKey(userID, generation)

	if s.cache != nil {
		if data, err := s.cache.Get(ctx, key); err == nil && data != nil {
			var snap snapshot
			if json.Unmarshal(data, &snap) == nil {
				return validateSnapshot(snap, tenantID, role, tokenVersion)
			}
		}
	}

	currentRole, currentTokenVersion, currentTenantID, tenantStatus, err := s.users.GetSessionState(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewUnauthorized("account is not active")
		}
		return apperror.NewInternal("failed to validate session")
	}

	snap := snapshot{
		Role:         currentRole,
		TokenVersion: currentTokenVersion,
		TenantID:     currentTenantID,
		TenantStatus: tenantStatus,
	}
	if s.cache != nil {
		if data, err := json.Marshal(snap); err == nil {
			if err := s.cache.Set(ctx, key, data, cacheTTL); err != nil {
				slog.Warn("failed to cache session snapshot",
					slog.Int64("user_id", userID),
					slog.Any("error", err),
				)
			}
		}
	}

	return validateSnapshot(snap, tenantID, role, tokenVersion)
}

func validateSnapshot(snap snapshot, tenantID int64, role string, tokenVersion int32) error {
	if snap.TenantStatus != "active" {
		return apperror.NewUnauthorized("tenant is not active")
	}
	if snap.TenantID != tenantID {
		return apperror.NewUnauthorized("session is no longer valid")
	}
	if snap.Role != role || snap.TokenVersion != tokenVersion {
		return apperror.NewUnauthorized("session is no longer valid")
	}
	return nil
}

func (s *store) Invalidate(ctx context.Context, userID int64) error {
	if s.cache == nil {
		return nil
	}
	if _, err := s.cache.Increment(ctx, generationKey(userID), generationTTL); err != nil {
		slog.Warn("failed to invalidate session cache",
			slog.Int64("user_id", userID),
			slog.Any("error", err),
		)
		return err
	}
	return nil
}
