package audit

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// insertEventSQL inserts one audit row. Empty optional values are stored as
// NULL so "absent" and "empty string" stay distinguishable in the trail.
const insertEventSQL = `
INSERT INTO audit_events (
    tenant_id,
    parent_id,
    actor_id,
    actor_type,
    actor_role,
    action,
    resource,
    resource_id,
    metadata,
    ip,
    user_agent,
    request_id,
    source,
    outcome,
    status_code,
    error_code,
    idempotency_key_hash,
    completed_at,
    created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`

const completeEventSQL = `
UPDATE audit_events
SET outcome = $2,
    status_code = $3,
    error_code = $4,
    completed_at = $5
WHERE id = $1
  AND source = 'http'
  AND outcome = 'started'`

// PostgresRecorder writes audit events to the audit_events table.
type PostgresRecorder struct {
	db database.DBTX
}

var (
	_ Recorder           = (*PostgresRecorder)(nil)
	_ TxRecorder         = (*PostgresRecorder)(nil)
	_ IDRecorder         = (*PostgresRecorder)(nil)
	_ CompletionRecorder = (*PostgresRecorder)(nil)
)

// NewPostgresRecorder returns a recorder backed by db. Production passes a
// database.Scoped handle so each row is written under its context's tenant.
func NewPostgresRecorder(db database.DBTX) *PostgresRecorder {
	return &PostgresRecorder{db: db}
}

type eventValues struct {
	tenantID   uuid.UUID
	parentID   *uuid.UUID
	actorID    *uuid.UUID
	actorType  string
	actorRole  *string
	metadata   []byte
	resourceID *string
	ip         *string
	userAgent  *string
	requestID  *string
	source     string
	outcome    string
	statusCode *int16
	errorCode  *string
	idemHash   *[]byte
	completed  *time.Time
	createdAt  time.Time
}

func (r *PostgresRecorder) values(ctx context.Context, event Event) (eventValues, error) {
	metadata, err := MarshalMetadata(event.Metadata)
	if err != nil {
		return eventValues{}, err
	}

	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	tenantID := event.TenantID
	if tenantID == uuid.Nil {
		required, err := tenant.Require(ctx)
		if err != nil {
			return eventValues{}, fmt.Errorf("audit: event has no tenant: %w", err)
		}
		tenantID = required
	}

	return eventValues{
		tenantID:   tenantID,
		parentID:   nullableUUID(event.ParentID),
		actorID:    nullableUUID(event.ActorID),
		actorType:  pgText(event.EffectiveActorType()),
		actorRole:  nullableString(event.ActorRole),
		metadata:   metadata,
		resourceID: nullableString(event.ResourceID),
		ip:         nullableIP(event.IP),
		userAgent:  nullableString(event.UserAgent),
		requestID:  nullableString(event.RequestID),
		source:     event.EffectiveSource(),
		outcome:    event.EffectiveOutcome(),
		statusCode: nullableStatusCode(event.StatusCode),
		errorCode:  nullableString(event.ErrorCode),
		idemHash:   nullableBytes(event.IdempotencyKeyHash),
		completed:  nullableTime(event.CompletedAt),
		createdAt:  createdAt,
	}, nil
}

// Record validates event, sanitizes its metadata, and inserts it through the
// pool.
//
// It returns an error wrapping [ErrInvalidEvent] for an invalid event,
// including a non-terminal outcome, and a wrapped marshaling error when
// Metadata cannot be encoded.
func (r *PostgresRecorder) Record(ctx context.Context, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	values, err := r.values(ctx, event)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, insertEventSQL, insertArgs(event, values)...)
	if err != nil {
		return fmt.Errorf("audit: insert event: %w", err)
	}
	return nil
}

// RecordTx is Record on a caller-owned transaction. A nil transaction is
// rejected with [ErrTxRequired]; the mutation must roll back in that case.
func (r *PostgresRecorder) RecordTx(ctx context.Context, tx pgx.Tx, event Event) error {
	if tx == nil {
		metrics.AuditTxMissingTotal.Inc()
		return ErrTxRequired
	}
	if err := event.Validate(); err != nil {
		return err
	}
	values, err := r.values(ctx, event)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertEventSQL, insertArgs(event, values)...); err != nil {
		return fmt.Errorf("audit: insert event in transaction: %w", err)
	}
	return nil
}

// RecordID inserts a pre-write intent row (source=http, outcome=started) and
// returns its ID. It is used by [Guard] before a Tier 2 handler runs.
func (r *PostgresRecorder) RecordID(ctx context.Context, event Event) (uuid.UUID, error) {
	if err := event.validateIntent(); err != nil {
		return uuid.Nil, err
	}
	event.Outcome = OutcomeStarted
	values, err := r.values(ctx, event)
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	if err := r.db.QueryRow(ctx, insertEventSQL+`
 RETURNING id`, insertArgs(event, values)...).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("audit: insert intent event: %w", err)
	}
	return id, nil
}

// Complete transitions a started HTTP intent row to a terminal outcome. Rows
// that are already completed, were not started, or are not HTTP events return
// [ErrAlreadyCompleted]; completions are immutable and happen at most once.
func (r *PostgresRecorder) Complete(ctx context.Context, id uuid.UUID, completion Completion) error {
	if id == uuid.Nil {
		return fmt.Errorf("%w: invalid event id", ErrInvalidEvent)
	}
	if err := completion.Validate(); err != nil {
		return err
	}
	if completion.Outcome == "" {
		completion.Outcome = OutcomeSucceeded
	}
	completedAt := completion.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	tag, err := r.db.Exec(ctx, completeEventSQL,
		id,
		completion.Outcome,
		nullableStatusCode(completion.StatusCode),
		nullableString(completion.ErrorCode),
		completedAt,
	)
	if err != nil {
		return fmt.Errorf("audit: complete event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyCompleted
	}
	return nil
}

func insertArgs(event Event, values eventValues) []any {
	return []any{
		values.tenantID,
		values.parentID,
		values.actorID,
		values.actorType,
		values.actorRole,
		pgText(event.Action),
		pgText(event.Resource),
		values.resourceID,
		values.metadata,
		values.ip,
		values.userAgent,
		values.requestID,
		values.source,
		values.outcome,
		values.statusCode,
		values.errorCode,
		values.idemHash,
		values.completed,
		values.createdAt,
	}
}

func nullableUUID(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	return &value
}

// nullableBytes stores an empty digest as NULL.
func nullableBytes(value []byte) *[]byte {
	if len(value) == 0 {
		return nil
	}
	return &value
}

// pgText makes text storable in a PostgreSQL text column. Request-derived
// fields (User-Agent, IP, request and resource ids) are client-controlled:
// invalid UTF-8 fails the insert with SQLSTATE 22021 and text cannot hold NUL,
// so without this a crafted header could turn every guarded mutation into a
// 503 or let a fallback-audited mutation succeed with no audit row.
func pgText(value string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(value, "\uFFFD"), "\x00", "")
}

// nullableString stores "" as NULL and sanitizes everything else with pgText.
func nullableString(value string) *string {
	value = pgText(value)
	if value == "" {
		return nil
	}
	return &value
}

// nullableIP returns the canonical form of an IP address for the INET column,
// or nil when value is empty or not an address. The IP comes from the
// request (and, behind a trusted proxy, from a client-controlled header), so
// a malformed value must neither fail the insert (an audit write failure is a
// 503 for guarded routes) nor be stored raw. An IPv6 zone ("fe80::1%eth0")
// is dropped because INET cannot hold it.
func nullableIP(value string) *string {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	canonical := addr.WithZone("").String()
	return &canonical
}

func nullableStatusCode(value int) *int16 {
	if value == 0 {
		return nil
	}
	if value < 1 || value > 599 {
		return nil
	}
	//nolint:gosec // HTTP status codes are validated to 100..599, which fits int16.
	v := int16(value)
	return &v
}

func nullableTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}
