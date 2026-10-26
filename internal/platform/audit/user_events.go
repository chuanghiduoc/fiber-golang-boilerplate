package audit

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
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

// UserEventReader reads a user's own audit rows for one tenant. audit_events
// is append-only and platform-owned, so the read lives here rather than in a
// module query.
type UserEventReader interface {
	ListUserEvents(ctx context.Context, tenantID, userID int64, limit int32) ([]UserEvent, error)
}

// PostgresUserEventReader implements UserEventReader over audit_events.
type PostgresUserEventReader struct {
	db database.DBTX
}

// NewUserEventReader returns a reader over db (the RLS-scoped handle).
func NewUserEventReader(db database.DBTX) *PostgresUserEventReader {
	return &PostgresUserEventReader{db: db}
}

const listUserAuditEventsSQL = `
SELECT action, created_at, COALESCE(host(ip), ''), COALESCE(user_agent, ''),
       resource, COALESCE(resource_id, ''), outcome
FROM audit_events
WHERE tenant_id = $1 AND actor_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3`

func (r *PostgresUserEventReader) ListUserEvents(ctx context.Context, tenantID, userID int64, limit int32) ([]UserEvent, error) {
	rows, err := r.db.Query(ctx, listUserAuditEventsSQL, tenantID, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]UserEvent, 0, limit)
	for rows.Next() {
		var event UserEvent
		if err := rows.Scan(&event.Action, &event.CreatedAt, &event.IP, &event.UserAgent,
			&event.Resource, &event.ResourceID, &event.Outcome); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

var _ UserEventReader = (*PostgresUserEventReader)(nil)
