package features

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Querier is the subset of pgx used by PostgresStore. *pgxpool.Pool and
// pgx.Tx both satisfy it, which keeps the store usable in tests and
// transactions.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PostgresStore loads the complete feature-flag snapshot from PostgreSQL. The
// feature_flags and feature_flag_targets tables are owned by this package; the
// admin module manages them through its own sqlc queries.
type PostgresStore struct {
	db Querier
}

// NewPostgresStore returns a store backed by db.
func NewPostgresStore(db Querier) *PostgresStore {
	return &PostgresStore{db: db}
}

var _ Store = (*PostgresStore)(nil)

// LoadSnapshot implements Store. Database failures are returned unwrapped by
// precedence logic; the evaluator decides whether a stale snapshot is still
// safe to use.
func (s *PostgresStore) LoadSnapshot(ctx context.Context) (Snapshot, error) {
	snapshot := Snapshot{
		Flags:         make(map[Key]FlagState),
		TenantTargets: make(map[Key]map[uuid.UUID]bool),
		UserTargets:   make(map[Key]map[UserTargetKey]bool),
	}

	flagRows, err := s.db.Query(ctx, `SELECT flag_key, enabled, kill_switch FROM feature_flags`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load feature flags: %w", err)
	}
	for flagRows.Next() {
		var (
			key        string
			enabled    pgtype.Bool
			killSwitch bool
		)
		if err := flagRows.Scan(&key, &enabled, &killSwitch); err != nil {
			flagRows.Close()
			return Snapshot{}, fmt.Errorf("load feature flags: scan row: %w", err)
		}
		state := FlagState{KillSwitch: killSwitch}
		if enabled.Valid {
			value := enabled.Bool
			state.Enabled = &value
		}
		snapshot.Flags[Key(key)] = state
	}
	if err := flagRows.Err(); err != nil {
		flagRows.Close()
		return Snapshot{}, fmt.Errorf("load feature flags: %w", err)
	}
	flagRows.Close()

	targetRows, err := s.db.Query(ctx,
		`SELECT flag_key, tenant_id, user_id, enabled FROM feature_flag_targets`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load feature flag targets: %w", err)
	}
	defer targetRows.Close()

	for targetRows.Next() {
		var (
			key      string
			tenantID uuid.UUID
			userID   uuid.NullUUID
			enabled  bool
		)
		if err := targetRows.Scan(&key, &tenantID, &userID, &enabled); err != nil {
			return Snapshot{}, fmt.Errorf("load feature flag targets: scan row: %w", err)
		}
		flagKey := Key(key)
		if userID.Valid {
			if snapshot.UserTargets[flagKey] == nil {
				snapshot.UserTargets[flagKey] = make(map[UserTargetKey]bool)
			}
			snapshot.UserTargets[flagKey][UserTargetKey{
				TenantID: tenantID,
				UserID:   userID.UUID,
			}] = enabled
			continue
		}
		if snapshot.TenantTargets[flagKey] == nil {
			snapshot.TenantTargets[flagKey] = make(map[uuid.UUID]bool)
		}
		snapshot.TenantTargets[flagKey][tenantID] = enabled
	}
	if err := targetRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("load feature flag targets: %w", err)
	}
	return snapshot, nil
}
