// Package session validates access-token claims against the current session,
// membership, user, and tenant state, so revocations, bans, role changes, and
// credential changes take effect immediately instead of waiting for the JWT
// to expire.
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

// Claims are the access-token claims the validator checks. Tokens minted
// before tenant-scoped RBAC may still carry a role claim; it is ignored.
type Claims struct {
	UserID       int64
	TenantID     int64
	SessionID    int64
	TokenVersion int32
}

// State is the current authorization state behind one session, as loaded by
// the identity module (see StateReader).
type State struct {
	// TenantID is the session's tenant; it must equal the token's claim.
	TenantID     int64
	Role         string
	Permissions  []string
	Platform     bool
	TokenVersion int32
	// ExpiresAt is the session's absolute lifetime; a session past it is dead
	// even before its access token expires.
	ExpiresAt time.Time
	// Active is true when the session is not revoked, the membership is
	// active, and the user is active.
	Active bool
	// TenantActive is true when the tenant serves requests.
	TenantActive bool
}

// StateReader is the narrow read contract the validator needs. Keeping it
// local prevents platform code from depending on a business module or its
// generated SQL types. Implementations return apperror.ErrNotFound for an
// unknown session or a session of another user.
type StateReader interface {
	SessionState(ctx context.Context, sessionID, userID int64) (State, error)
}

// Store validates a token against its session and returns the current state,
// including the effective permission set.
type Store interface {
	Validate(ctx context.Context, claims Claims) (State, error)
	// Invalidate drops every cached snapshot of the user's sessions.
	Invalidate(ctx context.Context, userID int64) error
	// InvalidatePermissions drops every cached permission snapshot.
	InvalidatePermissions(ctx context.Context) error
}

type snapshot struct {
	TenantID     int64     `json:"tenantId"`
	Role         string    `json:"role"`
	Permissions  []string  `json:"permissions"`
	Platform     bool      `json:"platform"`
	TokenVersion int32     `json:"tokenVersion"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Active       bool      `json:"active"`
	TenantActive bool      `json:"tenantActive"`
}

type store struct {
	states StateReader
	cache  cache.Cache
}

// NewStore creates a session validator. A cache is optional; without one every
// authenticated request performs a single indexed session lookup.
func NewStore(states StateReader, appCache cache.Cache) Store {
	return &store{states: states, cache: appCache}
}

func generationKey(userID int64) string {
	return fmt.Sprintf("session:generation:%d", userID)
}

const permissionsGenerationKey = "session:permissions_generation"

func snapshotKey(userID, generation, permissionGeneration, sessionID int64) string {
	return fmt.Sprintf("session:user:%d:%d:%d:%d", userID, generation, permissionGeneration, sessionID)
}

// currentGeneration reads a monotonically increasing counter. A snapshot is
// stored under the generations it was read at, so a slow request that finishes
// after an invalidation writes to an obsolete key instead of resurrecting a
// stale role, permission set, or token version.
func (s *store) currentGeneration(ctx context.Context, key string) int64 {
	if s.cache == nil {
		return 0
	}
	data, err := s.cache.Get(ctx, key)
	if err != nil || data == nil {
		return 0
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *store) Validate(ctx context.Context, claims Claims) (State, error) {
	if claims.TenantID <= 0 {
		return State{}, apperror.NewUnauthorized("token is missing tenant")
	}
	if claims.SessionID <= 0 {
		return State{}, apperror.NewUnauthorized("token is missing session")
	}
	generation := s.currentGeneration(ctx, generationKey(claims.UserID))
	permissionGeneration := s.currentGeneration(ctx, permissionsGenerationKey)
	key := snapshotKey(claims.UserID, generation, permissionGeneration, claims.SessionID)

	if s.cache != nil {
		if data, err := s.cache.Get(ctx, key); err == nil && data != nil {
			var snap snapshot
			if json.Unmarshal(data, &snap) == nil {
				return stateOf(snap, claims)
			}
		}
	}

	state, err := s.states.SessionState(ctx, claims.SessionID, claims.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return State{}, apperror.NewUnauthorized("session is no longer valid")
		}
		return State{}, apperror.NewInternal("failed to validate session")
	}

	snap := toSnapshot(state)
	if s.cache != nil {
		if data, err := json.Marshal(snap); err == nil {
			if err := s.cache.Set(ctx, key, data, cacheTTL); err != nil {
				slog.Warn("failed to cache session snapshot",
					slog.Int64("user_id", claims.UserID),
					slog.Any("error", err),
				)
			}
		}
	}

	return stateOf(snap, claims)
}

func toSnapshot(state State) snapshot {
	return snapshot(state)
}

func stateOf(snap snapshot, claims Claims) (State, error) {
	state := State(snap)
	if !snap.TenantActive {
		return State{}, apperror.NewUnauthorized("tenant is not active")
	}
	if snap.TenantID != claims.TenantID || !snap.Active {
		return State{}, apperror.NewUnauthorized("session is no longer valid")
	}
	if snap.TokenVersion != claims.TokenVersion {
		return State{}, apperror.NewUnauthorized("session is no longer valid")
	}
	if snap.ExpiresAt.IsZero() || !snap.ExpiresAt.After(time.Now()) {
		return State{}, apperror.NewUnauthorized("session has expired")
	}
	return state, nil
}

func (s *store) Invalidate(ctx context.Context, userID int64) error {
	return s.bump(ctx, generationKey(userID))
}

func (s *store) InvalidatePermissions(ctx context.Context) error {
	return s.bump(ctx, permissionsGenerationKey)
}

func (s *store) bump(ctx context.Context, key string) error {
	if s.cache == nil {
		return nil
	}
	if _, err := s.cache.Increment(ctx, key, generationTTL); err != nil {
		slog.Warn("failed to invalidate session cache", slog.String("key", key), slog.Any("error", err))
		return err
	}
	return nil
}
