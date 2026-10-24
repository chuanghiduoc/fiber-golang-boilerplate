package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TestPostgresRecorderIntegration inserts an event and reads it back. It is
// gated on TEST_DATABASE_URL so the normal unit-test run stays Docker-free:
// without the variable the test skips, and with it the target database must
// already have the schema applied (make migrate-run or cmd/migrate).
func TestPostgresRecorderIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	var tableExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&tableExists))
	if !tableExists {
		t.Skip("audit_events table not found; run cmd/migrate first")
	}

	requestID := fmt.Sprintf("audit-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteTestAuditRows(t, pool, requestID) })

	recorder := NewPostgresRecorder(pool)
	wantMetadata := map[string]any{
		"reason": "admin-request",
		"admin":  true,
	}
	wantCreatedAt := time.Now().UTC().Truncate(time.Microsecond)

	err = recorder.Record(ctx, Event{
		ActorID:    42,
		Action:     "user.role_changed",
		Resource:   "user",
		ResourceID: "7",
		Metadata:   wantMetadata,
		IP:         "203.0.113.10",
		UserAgent:  "audit-integration-test",
		RequestID:  requestID,
		CreatedAt:  wantCreatedAt,
	})
	require.NoError(t, err)

	var (
		tenantID   int64
		actorID    *int64
		action     string
		resource   string
		resourceID *string
		metadata   []byte
		ip         *string
		userAgent  *string
		gotRequest *string
		createdAt  time.Time
	)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT tenant_id, actor_id, action, resource, resource_id, metadata, ip, user_agent, request_id, created_at
		FROM audit_events
		WHERE request_id = $1`, requestID,
	).Scan(&tenantID, &actorID, &action, &resource, &resourceID, &metadata, &ip, &userAgent, &gotRequest, &createdAt))

	assert.Equal(t, tenant.DefaultID, tenantID)
	require.NotNil(t, actorID)
	assert.Equal(t, int64(42), *actorID)
	assert.Equal(t, "user.role_changed", action)
	assert.Equal(t, "user", resource)
	require.NotNil(t, resourceID)
	assert.Equal(t, "7", *resourceID)
	require.NotNil(t, ip)
	assert.Equal(t, "203.0.113.10", *ip)
	require.NotNil(t, userAgent)
	assert.Equal(t, "audit-integration-test", *userAgent)
	require.NotNil(t, gotRequest)
	assert.Equal(t, requestID, *gotRequest)
	assert.True(t, wantCreatedAt.Equal(createdAt), "created_at = %s, want %s", createdAt, wantCreatedAt)

	var gotMetadata map[string]any
	require.NoError(t, json.Unmarshal(metadata, &gotMetadata))
	assert.Equal(t, wantMetadata, gotMetadata)
}

// TestPostgresRecorderAcceptsHostileText proves client-controlled text with
// invalid UTF-8 or NUL bytes is stored instead of failing the insert (22021),
// which would 503 guarded routes and drop fallback audit rows.
func TestPostgresRecorderAcceptsHostileText(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	requestID := fmt.Sprintf("audit-hostile-%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteTestAuditRows(t, pool, requestID) })

	recorder := NewPostgresRecorder(pool)
	require.NoError(t, recorder.Record(ctx, Event{
		Action:     "POST /files",
		Resource:   "/files",
		ResourceID: "id\xff\x00",
		Metadata:   map[string]any{"note": "nul\x00here\xfe"},
		IP:         "203.0.113.10\xff",
		UserAgent:  "bad\xff\x00agent",
		RequestID:  requestID,
	}))

	var userAgent, resourceID string
	var metadata []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT user_agent, resource_id, metadata FROM audit_events WHERE request_id = $1`, requestID,
	).Scan(&userAgent, &resourceID, &metadata))
	assert.Equal(t, "bad�agent", userAgent)
	assert.Equal(t, "id�", resourceID)
	assert.JSONEq(t, `{"note":"nulhere"}`, string(metadata))
}

// TestPostgresRecorderIntegrationTenant writes tenant-scoped rows and verifies
// that the recorder resolves a missing Event.TenantID from the context.
func TestPostgresRecorderIntegrationTenant(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	var tableExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_regclass('audit_events') IS NOT NULL AND to_regclass('tenants') IS NOT NULL`).
		Scan(&tableExists))
	if !tableExists {
		t.Skip("audit_events/tenants tables not found; run cmd/migrate first")
	}

	slug := fmt.Sprintf("audit-tenant-%d", time.Now().UnixNano())
	var tenantID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, 'Audit Tenant') RETURNING id`, slug).
		Scan(&tenantID))

	requestID := fmt.Sprintf("audit-tenant-test-%d", time.Now().UnixNano())
	ctxRequestID := requestID + "-ctx"
	t.Cleanup(func() {
		deleteTestAuditRows(t, pool, requestID, ctxRequestID)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID); err != nil {
			t.Errorf("cleanup tenant: %v", err)
		}
	})

	recorder := NewPostgresRecorder(pool)
	require.NoError(t, recorder.Record(ctx, Event{
		TenantID:   tenantID,
		Action:     "tenant.settings_changed",
		Resource:   "tenant",
		ResourceID: fmt.Sprint(tenantID),
		RequestID:  requestID,
	}))

	// Explicit domain events may omit the tenant; the context supplies it.
	require.NoError(t, recorder.Record(tenant.WithID(ctx, tenantID), Event{
		Action:    "tenant.export_requested",
		Resource:  "tenant",
		RequestID: ctxRequestID,
	}))

	var gotTenant int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM audit_events WHERE request_id = $1`, requestID).Scan(&gotTenant))
	assert.Equal(t, tenantID, gotTenant, "an explicit Event.TenantID must be stored")

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM audit_events WHERE request_id = $1`, ctxRequestID).Scan(&gotTenant))
	assert.Equal(t, tenantID, gotTenant,
		"a zero Event.TenantID must be resolved from the recording context")
}

// TestPostgresRecorderIntegrationDefaults covers the nullable columns and the
// default metadata/created_at handling against a real database.
func TestPostgresRecorderIntegrationDefaults(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	var tableExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&tableExists))
	if !tableExists {
		t.Skip("audit_events table not found; run cmd/migrate first")
	}

	requestID := fmt.Sprintf("audit-test-defaults-%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteTestAuditRows(t, pool, requestID) })

	before := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, NewPostgresRecorder(pool).Record(ctx, Event{
		Action:    "system.cleanup",
		Resource:  "job",
		RequestID: requestID,
	}))

	var (
		tenantID   int64
		actorID    *int64
		resourceID *string
		ip         *string
		userAgent  *string
		metadata   []byte
		createdAt  time.Time
	)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT tenant_id, actor_id, resource_id, ip, user_agent, metadata, created_at
		FROM audit_events
		WHERE request_id = $1`, requestID,
	).Scan(&tenantID, &actorID, &resourceID, &ip, &userAgent, &metadata, &createdAt))

	assert.Equal(t, tenant.DefaultID, tenantID)
	assert.Nil(t, actorID)
	assert.Nil(t, resourceID)
	assert.Nil(t, ip)
	assert.Nil(t, userAgent)
	assert.JSONEq(t, `{}`, string(metadata))
	assert.WithinDuration(t, before, createdAt, 2*time.Minute)
}

// TestPostgresRecorderRecordTxRollback proves a transactional audit write
// disappears with the mutation when the transaction rolls back.
func TestPostgresRecorderRecordTxRollback(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	var tableExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&tableExists))
	if !tableExists {
		t.Skip("audit_events table not found; run cmd/migrate first")
	}

	requestID := fmt.Sprintf("audit-tx-rollback-%d", time.Now().UnixNano())
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, NewPostgresRecorder(pool).RecordTx(ctx, tx, Event{
		Action:    "user.role_changed",
		Resource:  "user",
		Source:    SourceService,
		Outcome:   OutcomeSucceeded,
		RequestID: requestID,
	}))
	require.NoError(t, tx.Rollback(ctx))

	var count int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE request_id = $1`, requestID).Scan(&count))
	assert.Zero(t, count, "a rolled-back transaction must leave no audit row")
}

// TestPostgresRecorderCompleteOnce covers intent creation, completion fields,
// one-shot semantics, and the completion-only trigger.
func TestPostgresRecorderCompleteOnce(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping audit integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	requestID := fmt.Sprintf("audit-complete-%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteTestAuditRows(t, pool, requestID, requestID+"-service") })

	recorder := NewPostgresRecorder(pool)
	id, err := recorder.RecordID(ctx, Event{
		ActorType:  ActorTypeAnonymous,
		Action:     "auth.login",
		Resource:   "auth",
		RequestID:  requestID,
		Source:     SourceHTTP,
		Outcome:    OutcomeStarted,
		StatusCode: 0,
	})
	require.NoError(t, err)
	require.Positive(t, id)

	var (
		outcome     string
		completedAt *time.Time
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT outcome, completed_at FROM audit_events WHERE id = $1`, id).
		Scan(&outcome, &completedAt))
	assert.Equal(t, OutcomeStarted, outcome)
	assert.Nil(t, completedAt)

	require.NoError(t, recorder.Complete(ctx, id, Completion{
		ActorID:    42,
		ActorType:  ActorTypeAdmin,
		ActorRole:  "admin",
		Outcome:    OutcomeSucceeded,
		StatusCode: 200,
	}))

	var (
		actorID    *int64
		actorType  string
		actorRole  *string
		statusCode *int16
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT outcome, completed_at, actor_id, actor_type, actor_role, status_code
		 FROM audit_events WHERE id = $1`, id).
		Scan(&outcome, &completedAt, &actorID, &actorType, &actorRole, &statusCode))
	assert.Equal(t, OutcomeSucceeded, outcome)
	require.NotNil(t, completedAt)
	require.NotNil(t, actorID)
	assert.Equal(t, int64(42), *actorID)
	assert.Equal(t, ActorTypeAdmin, actorType)
	require.NotNil(t, actorRole)
	assert.Equal(t, "admin", *actorRole)
	require.NotNil(t, statusCode)
	assert.Equal(t, int16(200), *statusCode)

	err = recorder.Complete(ctx, id, Completion{Outcome: OutcomeFailed, StatusCode: 500})
	require.ErrorIs(t, err, ErrAlreadyCompleted, "completion must happen at most once")

	_, err = pool.Exec(ctx,
		`UPDATE audit_events SET action = 'tampered' WHERE id = $1`, id)
	require.Error(t, err, "completed rows must reject non-completion updates")

	// A terminal service row is not a completable HTTP intent.
	require.NoError(t, recorder.Record(ctx, Event{
		Action:    "user.updated",
		Resource:  "user",
		RequestID: requestID + "-service",
		Source:    SourceService,
		Outcome:   OutcomeSucceeded,
	}))
	var serviceID int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM audit_events WHERE request_id = $1`, requestID+"-service").Scan(&serviceID))
	err = recorder.Complete(ctx, serviceID, Completion{Outcome: OutcomeFailed, StatusCode: 500})
	require.ErrorIs(t, err, ErrAlreadyCompleted)

	_, err = pool.Exec(ctx,
		`UPDATE audit_events SET action = 'tampered' WHERE id = $1`, serviceID)
	require.Error(t, err, "terminal rows must reject updates")

	// Append-only covers removal too: neither DELETE nor TRUNCATE may drop
	// audit rows outside a reviewed, trigger-disabling maintenance path.
	_, err = pool.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, serviceID)
	require.ErrorContains(t, err, "append-only", "audit rows must reject DELETE")
	// Inside a rolled-back transaction so a missing trigger cannot wipe a
	// shared test database.
	truncateTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = truncateTx.Exec(ctx, `TRUNCATE audit_events`)
	require.NoError(t, truncateTx.Rollback(ctx))
	require.ErrorContains(t, err, "append-only", "audit_events must reject TRUNCATE")
	var remaining int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE id IN ($1, $2)`, id, serviceID).Scan(&remaining))
	assert.Equal(t, int64(2), remaining)
}

// deleteTestAuditRows removes this test's rows from a shared TEST_DATABASE_URL
// database. audit_events rejects DELETE, so the cleanup disables the
// append-only row trigger inside one transaction (requires table ownership,
// like running migrations); other sessions never observe it disabled.
func deleteTestAuditRows(t *testing.T, pool *pgxpool.Pool, requestIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Errorf("cleanup audit rows: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER trigger_audit_events_no_delete`); err != nil {
		t.Errorf("cleanup audit rows: disable trigger: %v", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE request_id = ANY($1)`, requestIDs); err != nil {
		t.Errorf("cleanup audit rows: delete: %v", err)
		return
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER trigger_audit_events_no_delete`); err != nil {
		t.Errorf("cleanup audit rows: enable trigger: %v", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("cleanup audit rows: commit: %v", err)
	}
}
