package audit

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
)

// UserEvent is one audit row a user produced in one tenant, for a GDPR
// export. It deliberately excludes metadata, request ids, and idempotency
// hashes: action, time, address, client, and resource are the export fields.
type UserEvent struct {
	Action     string
	CreatedAt  time.Time
	IP         string
	UserAgent  string
	Resource   string
	ResourceID string
	Outcome    string
}

// UserEventPage is one bounded audit read. Truncated reports that at least one
// older row exists beyond the requested limit.
type UserEventPage struct {
	Events    []UserEvent
	Truncated bool
}

// UserEventReader reads a user's audit rows for one tenant. audit_events is
// append-only and platform-owned, so the read lives here rather than in a
// module query.
type UserEventReader interface {
	// ListUserEvents returns the rows the user performed themselves: the
	// actor is the user.
	ListUserEvents(ctx context.Context, tenantID, userID uuid.UUID, limit int32) (UserEventPage, error)
	// LatestUserActorEvent returns the newest row the user performed with the
	// given action. Status lookups use it instead of scanning a bounded page,
	// so later unrelated activity cannot hide the row they need.
	LatestUserActorEvent(ctx context.Context, tenantID, userID uuid.UUID, action string) (UserEvent, bool, error)
	// LatestUserTargetEvent returns the newest row whose resource is the user,
	// whoever performed it. An admin erasure records the admin as actor and the
	// member as resource_id, so a member status lookup cannot filter on the
	// actor.
	LatestUserTargetEvent(ctx context.Context, tenantID, userID uuid.UUID, action string) (UserEvent, bool, error)
}

// PostgresUserEventReader implements UserEventReader over audit_events.
type PostgresUserEventReader struct {
	db database.DBTX
}

// NewUserEventReader returns a reader over db (the RLS-scoped handle).
func NewUserEventReader(db database.DBTX) *PostgresUserEventReader {
	return &PostgresUserEventReader{db: db}
}

const userAuditEventColumns = `action, created_at, COALESCE(host(ip), ''), COALESCE(user_agent, ''),
       resource, COALESCE(resource_id, ''), outcome`

const listUserAuditEventsSQL = `
SELECT ` + userAuditEventColumns + `
FROM audit_events
WHERE tenant_id = $1 AND actor_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3`

const latestUserActorAuditEventSQL = `
SELECT ` + userAuditEventColumns + `
FROM audit_events
WHERE tenant_id = $1 AND actor_id = $2 AND action = $3
ORDER BY created_at DESC, id DESC
LIMIT 1`

const latestUserTargetAuditEventSQL = `
SELECT ` + userAuditEventColumns + `
FROM audit_events
WHERE tenant_id = $1 AND resource = 'user' AND resource_id = $2::text AND action = $3
ORDER BY created_at DESC, id DESC
LIMIT 1`

func (r *PostgresUserEventReader) ListUserEvents(ctx context.Context, tenantID, userID uuid.UUID, limit int32) (UserEventPage, error) {
	if limit <= 0 {
		return UserEventPage{}, nil
	}
	rows, err := r.db.Query(ctx, listUserAuditEventsSQL, tenantID, userID, limit+1)
	if err != nil {
		return UserEventPage{}, err
	}
	return scanUserEvents(rows, limit)
}

func (r *PostgresUserEventReader) LatestUserActorEvent(ctx context.Context, tenantID, userID uuid.UUID, action string) (UserEvent, bool, error) {
	return scanLatestUserEvent(r.db.QueryRow(ctx, latestUserActorAuditEventSQL, tenantID, userID, action))
}

func (r *PostgresUserEventReader) LatestUserTargetEvent(ctx context.Context, tenantID, userID uuid.UUID, action string) (UserEvent, bool, error) {
	return scanLatestUserEvent(r.db.QueryRow(ctx, latestUserTargetAuditEventSQL, tenantID, userID, action))
}

// scanLatestUserEvent reads a single-row lookup. The query already narrows by
// tenant and action, so an empty result means the event never happened.
func scanLatestUserEvent(row pgx.Row) (UserEvent, bool, error) {
	var event UserEvent
	err := row.Scan(
		&event.Action, &event.CreatedAt, &event.IP, &event.UserAgent,
		&event.Resource, &event.ResourceID, &event.Outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserEvent{}, false, nil
	}
	if err != nil {
		return UserEvent{}, false, err
	}
	return event, true, nil
}

func scanUserEvents(rows pgx.Rows, limit int32) (UserEventPage, error) {
	defer rows.Close()

	events := make([]UserEvent, 0, limit+1)
	for rows.Next() {
		var event UserEvent
		if err := rows.Scan(&event.Action, &event.CreatedAt, &event.IP, &event.UserAgent,
			&event.Resource, &event.ResourceID, &event.Outcome); err != nil {
			return UserEventPage{}, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return UserEventPage{}, err
	}
	truncated := len(events) > int(limit)
	if truncated {
		events = events[:limit]
	}
	return UserEventPage{Events: events, Truncated: truncated}, nil
}

var _ UserEventReader = (*PostgresUserEventReader)(nil)
