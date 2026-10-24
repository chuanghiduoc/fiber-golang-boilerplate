package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// insertEventSQL inserts one audit row. Empty optional values are stored as
// NULL so "absent" and "empty string" stay distinguishable in the trail.
const insertEventSQL = `
INSERT INTO audit_events (
    tenant_id,
    actor_id,
    action,
    resource,
    resource_id,
    metadata,
    ip,
    user_agent,
    request_id,
    created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// PostgresRecorder writes audit events to the audit_events table.
type PostgresRecorder struct {
	pool *pgxpool.Pool
}

var _ Recorder = (*PostgresRecorder)(nil)

// NewPostgresRecorder returns a recorder backed by pool.
func NewPostgresRecorder(pool *pgxpool.Pool) *PostgresRecorder {
	return &PostgresRecorder{pool: pool}
}

// Record validates event, marshals its metadata, and inserts it.
//
// It returns an error wrapping [ErrInvalidEvent] when Action or Resource is
// empty, and a wrapped marshaling error when Metadata cannot be encoded.
func (r *PostgresRecorder) Record(ctx context.Context, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}

	metadata, err := MarshalMetadata(event.Metadata)
	if err != nil {
		return err
	}

	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	tenantID := event.TenantID
	if tenantID == 0 {
		tenantID = tenant.EffectiveID(ctx)
	}
	if tenantID <= 0 {
		tenantID = tenant.DefaultID
	}

	_, err = r.pool.Exec(ctx, insertEventSQL,
		tenantID,
		nullableInt64(event.ActorID),
		event.Action,
		event.Resource,
		nullableString(event.ResourceID),
		metadata,
		nullableString(event.IP),
		nullableString(event.UserAgent),
		nullableString(event.RequestID),
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("audit: insert event: %w", err)
	}
	return nil
}

// Validate reports whether the event has the fields required by the schema.
func (e Event) Validate() error {
	if strings.TrimSpace(e.Action) == "" {
		return fmt.Errorf("%w: action is required", ErrInvalidEvent)
	}
	if strings.TrimSpace(e.Resource) == "" {
		return fmt.Errorf("%w: resource is required", ErrInvalidEvent)
	}
	return nil
}

// MarshalMetadata encodes metadata for a JSONB column. A nil map becomes an
// empty JSON object so the NOT NULL column keeps its documented default shape.
func MarshalMetadata(metadata map[string]any) ([]byte, error) {
	if len(metadata) == 0 {
		return []byte("{}"), nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal metadata: %w", err)
	}
	return encoded, nil
}

func nullableInt64(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
