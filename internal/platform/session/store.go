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
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const (
	cacheTTL         = 5 * time.Second
	generationTTL    = 24 * time.Hour
	stateReadTimeout = 5 * time.Second
)

// Claims are the access-token claims the validator checks. Tokens minted
// before tenant-scoped RBAC may still carry a role claim; it is ignored.
type Claims struct {
	UserID       uuid.UUID
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	TokenVersion int32
}

// State is the current authorization state behind one session, as loaded by
// the identity module (see StateReader).
type State struct {
	// TenantID is the session's tenant; it must equal the token's claim.
	TenantID     uuid.UUID
	Role         string
	Permissions  []string
	Platform     bool
	TokenVersion int32
	// ExpiresAt is the session's absolute lifetime; a session past it is dead
	// even before its access token expires.
	ExpiresAt time.Time
	// MFAVerifiedAt is when the session's second factor was last verified (MFA
	// login or step-up); the zero time means never. AAL2-gated routes compare
	// it with MFA_MAX_AGE.
	MFAVerifiedAt time.Time
	// AMR is the session's current authentication-method set. It is
	// authoritative for step-up: a passkey step-up adds hwk/swk to the live
	// session, and the validator unions it with the token snapshot.
	AMR []string
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
	SessionState(ctx context.Context, tenantID, sessionID, userID uuid.UUID) (State, error)
}

// Store validates a token against its session and returns the current state,
// including the effective permission set.
type Store interface {
	Validate(ctx context.Context, claims Claims) (State, error)
	// Invalidate drops every cached snapshot of the user's sessions.
	Invalidate(ctx context.Context, userID uuid.UUID) error
	// InvalidatePermissions drops every cached permission snapshot.
	InvalidatePermissions(ctx context.Context) error
}

type snapshot struct {
	TenantID      uuid.UUID `json:"tenantId"`
	Role          string    `json:"role"`
	Permissions   []string  `json:"permissions"`
	Platform      bool      `json:"platform"`
	TokenVersion  int32     `json:"tokenVersion"`
	ExpiresAt     time.Time `json:"expiresAt"`
	MFAVerifiedAt time.Time `json:"mfaVerifiedAt"`
	AMR           []string  `json:"amr,omitempty"`
	Active        bool      `json:"active"`
	TenantActive  bool      `json:"tenantActive"`
}

type store struct {
	states  StateReader
	cache   cache.Cache
	flights singleflight.Group
}

// NewStore creates a session validator. A cache is optional; without one every
// authenticated request performs a single indexed session lookup.
func NewStore(states StateReader, appCache cache.Cache) Store {
	return &store{states: states, cache: appCache}
}

func generationKey(userID uuid.UUID) string {
	return "session:generation:" + userID.String()
}

const permissionsGenerationKey = "session:permissions_generation"

func snapshotKey(userID uuid.UUID, generation, permissionGeneration int64, sessionID uuid.UUID) string {
	return fmt.Sprintf("session:user:%s:%d:%d:%s", userID, generation, permissionGeneration, sessionID)
}

// currentGeneration reads a monotonically increasing counter. A snapshot is
// stored under the generations it was read at, so a slow request that finishes
// after an invalidation writes to an obsolete key instead of resurrecting a
// stale role, permission set, or token version. A missing counter means zero;
// a failed or corrupt read is returned so the caller can bypass the cache
// instead of accidentally serving a snapshot stored under generation zero.
func (s *store) currentGeneration(ctx context.Context, key string) (int64, error) {
	if s.cache == nil {
		return 0, nil
	}
	data, err := s.cache.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if data == nil {
		return 0, nil
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid session generation at %q", key)
	}
	return n, nil
}

func (s *store) Validate(ctx context.Context, claims Claims) (State, error) {
	if claims.UserID == uuid.Nil {
		return State{}, apperror.NewUnauthorized("token is missing subject")
	}
	if claims.TenantID == uuid.Nil {
		return State{}, apperror.NewUnauthorized("token is missing tenant")
	}
	if claims.SessionID == uuid.Nil {
		return State{}, apperror.NewUnauthorized("token is missing session")
	}
	generation, err := s.currentGeneration(ctx, generationKey(claims.UserID))
	if err != nil {
		slog.Warn("failed to read session generation; bypassing cache",
			slog.String("user_id", claims.UserID.String()),
			slog.Any("error", err),
		)
		return s.validateUncached(ctx, claims)
	}
	permissionGeneration, err := s.currentGeneration(ctx, permissionsGenerationKey)
	if err != nil {
		slog.Warn("failed to read permission generation; bypassing cache",
			slog.Any("error", err),
		)
		return s.validateUncached(ctx, claims)
	}
	key := snapshotKey(claims.UserID, generation, permissionGeneration, claims.SessionID)

	if s.cache != nil {
		if data, err := s.cache.Get(ctx, key); err == nil && data != nil {
			var snap snapshot
			if json.Unmarshal(data, &snap) == nil {
				return stateOf(snap, claims)
			}
		}
	}

	snap, err := s.loadSnapshot(ctx, key, claims)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return State{}, apperror.NewUnauthorized("session is no longer valid")
		}
		return State{}, apperror.NewInternal("failed to validate session")
	}
	return stateOf(snap, claims)
}

// validateUncached reads the authoritative state directly when the cache
// generations cannot be trusted. It deliberately neither reads nor writes a
// snapshot: a cache outage must never turn into authorization from stale data.
func (s *store) validateUncached(ctx context.Context, claims Claims) (State, error) {
	readCtx, cancel := stateReadContext(ctx)
	defer cancel()
	state, err := s.states.SessionState(readCtx, claims.TenantID, claims.SessionID, claims.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return State{}, apperror.NewUnauthorized("session is no longer valid")
		}
		return State{}, apperror.NewInternal("failed to validate session")
	}
	return stateOf(toSnapshot(state), claims)
}

// loadSnapshot coalesces a burst of requests for the same session into one
// reader call. The generation and permission generation are part of key, so a
// result that finishes after an invalidation is cached under the obsolete key
// and cannot resurrect a revoked session or stale permissions.
func (s *store) loadSnapshot(ctx context.Context, key string, claims Claims) (snapshot, error) {
	value, err, _ := s.flights.Do(key, func() (any, error) {
		readCtx, cancel := stateReadContext(ctx)
		defer cancel()
		state, err := s.states.SessionState(readCtx, claims.TenantID, claims.SessionID, claims.UserID)
		if err != nil {
			return nil, err
		}
		snap := toSnapshot(state)
		if s.cache == nil {
			return snap, nil
		}
		data, err := json.Marshal(snap)
		if err != nil {
			return snap, nil
		}
		if err := s.cache.Set(readCtx, key, data, cacheTTL); err != nil {
			slog.Warn("failed to cache session snapshot",
				slog.String("user_id", claims.UserID.String()),
				slog.Any("error", err),
			)
		}
		return snap, nil
	})
	if err != nil {
		return snapshot{}, err
	}
	snap, ok := value.(snapshot)
	if !ok {
		return snapshot{}, apperror.NewInternal("failed to validate session")
	}
	return snap, nil
}

// stateReadContext detaches an authorization read from client cancellation so
// an aborted request cannot interrupt a mutation cache invalidation, while
// still bounding the database/cache call. WithoutCancel alone has no deadline.
func stateReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stateReadTimeout)
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
	// Singleflight waiters receive the same snapshot value. Clone its slices so
	// one caller cannot mutate the authorization state observed by another.
	state.Permissions = slices.Clone(snap.Permissions)
	state.AMR = slices.Clone(snap.AMR)
	return state, nil
}

func (s *store) Invalidate(ctx context.Context, userID uuid.UUID) error {
	return s.bump(ctx, generationKey(userID))
}

func (s *store) InvalidatePermissions(ctx context.Context) error {
	return s.bump(ctx, permissionsGenerationKey)
}

func (s *store) bump(ctx context.Context, key string) error {
	if s.cache == nil {
		return nil
	}
	// A client disconnect must not skip a revocation. Detach from the request
	// cancellation and keep a short deadline so a cache outage cannot hang the
	// mutation path.
	invalidationCtx, cancel := stateReadContext(ctx)
	defer cancel()
	if _, err := s.cache.Increment(invalidationCtx, key, generationTTL); err != nil {
		slog.Warn("failed to invalidate session cache", slog.String("key", key), slog.Any("error", err))
		return err
	}
	return nil
}
