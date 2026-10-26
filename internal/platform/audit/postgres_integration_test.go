//go:build integration

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// setupAuditTestDB gives each test its own migrated schema and a pool
// connected as the non-superuser app role, so the recorder runs with RLS
// enforced. Dropping the schema removes the rows; audit_events rejects DELETE.
func setupAuditTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := setupAuditTestDBWithOwner(t)
	return pool
}

// setupAuditTestDBWithOwner also returns a pool connected as the schema owner.
// A test that must reach a trigger on a statement the app role no longer has
// (TRUNCATE) uses it; application assertions stay on the app pool.
func setupAuditTestDBWithOwner(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, owner, cleanup, err := pgtest.SetupTestDBWithOwner(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return pool, owner
}

// TestPostgresRecorderIntegration inserts an event and reads it back.
func TestPostgresRecorderIntegration(t *testing.T) {
	pool := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-test-%d", time.Now().UnixNano())

	recorder := NewPostgresRecorder(pgtest.Scoped(pool))
	wantMetadata := map[string]any{
		"reason": "admin-request",
		"admin":  true,
	}
	wantCreatedAt := time.Now().UTC().Truncate(time.Microsecond)

	err := recorder.Record(ctx, Event{
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
		SELECT tenant_id, actor_id, action, resource, resource_id, metadata, host(ip), user_agent, request_id, created_at
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
	pool := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-hostile-%d", time.Now().UnixNano())

	recorder := NewPostgresRecorder(pgtest.Scoped(pool))
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
	pool := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	slug := fmt.Sprintf("audit-tenant-%d", time.Now().UnixNano())
	var tenantID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, 'Audit Tenant') RETURNING id`, slug).
		Scan(&tenantID))

	requestID := fmt.Sprintf("audit-tenant-test-%d", time.Now().UnixNano())
	ctxRequestID := requestID + "-ctx"
	ctxTenant := tenant.WithID(ctx, tenantID)

	recorder := NewPostgresRecorder(pgtest.Scoped(pool))
	require.NoError(t, recorder.Record(ctxTenant, Event{
		TenantID:   tenantID,
		Action:     "tenant.settings_changed",
		Resource:   "tenant",
		ResourceID: fmt.Sprint(tenantID),
		RequestID:  requestID,
	}))

	// Explicit domain events may omit the tenant; the context supplies it.
	require.NoError(t, recorder.Record(ctxTenant, Event{
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
	pool := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-test-defaults-%d", time.Now().UnixNano())

	before := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, NewPostgresRecorder(pgtest.Scoped(pool)).Record(ctx, Event{
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
	pool := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-tx-rollback-%d", time.Now().UnixNano())
	recorder := NewPostgresRecorder(pgtest.Scoped(pool))
	rolledBack := errors.New("roll back")
	err := pgtest.NewTxManager(pool).WithTx(ctx, func(tx pgx.Tx) error {
		if err := recorder.RecordTx(ctx, tx, Event{
			Action:    "user.role_changed",
			Resource:  "user",
			Source:    SourceService,
			Outcome:   OutcomeSucceeded,
			RequestID: requestID,
		}); err != nil {
			return err
		}
		return rolledBack
	})
	require.ErrorIs(t, err, rolledBack)

	var count int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE request_id = $1`, requestID).Scan(&count))
	assert.Zero(t, count, "a rolled-back transaction must leave no audit row")
}

// TestPostgresRecorderCompleteOnce covers intent creation, completion fields,
// one-shot semantics, and the completion-only trigger.
func TestPostgresRecorderCompleteOnce(t *testing.T) {
	pool, owner := setupAuditTestDBWithOwner(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-complete-%d", time.Now().UnixNano())

	recorder := NewPostgresRecorder(pgtest.Scoped(pool))
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
	assert.Nil(t, actorID, "completion must not invent an actor")
	assert.Equal(t, ActorTypeAnonymous, actorType, "completion must not change the intent actor")
	assert.Nil(t, actorRole)
	require.NotNil(t, statusCode)
	assert.Equal(t, int16(200), *statusCode)

	err = recorder.Complete(ctx, id, Completion{Outcome: OutcomeFailed, StatusCode: 500})
	require.ErrorIs(t, err, ErrAlreadyCompleted, "completion must happen at most once")

	_, err = pool.Exec(ctx,
		`UPDATE audit_events SET action = 'tampered' WHERE id = $1`, id)
	require.Error(t, err, "completed rows must reject non-completion updates")

	// A guarded request that was already authenticated records the same actor
	// on the intent and the completion; completing it as another actor is
	// refused by the trigger.
	sameActorID, err := recorder.RecordID(ctx, Event{
		ActorID:   42,
		ActorType: ActorTypeUser,
		ActorRole: "admin",
		Action:    "user.updated",
		Resource:  "user",
		RequestID: requestID + "-same-actor",
		Source:    SourceHTTP,
		Outcome:   OutcomeStarted,
	})
	require.NoError(t, err)
	require.NoError(t, recorder.Complete(ctx, sameActorID, Completion{
		ActorID:    42,
		ActorType:  ActorTypeUser,
		ActorRole:  "admin",
		Outcome:    OutcomeSucceeded,
		StatusCode: 200,
	}))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT actor_id, actor_type, actor_role FROM audit_events WHERE id = $1`, sameActorID).
		Scan(&actorID, &actorType, &actorRole))
	require.NotNil(t, actorID)
	assert.Equal(t, int64(42), *actorID)
	assert.Equal(t, ActorTypeUser, actorType)
	require.NotNil(t, actorRole)
	assert.Equal(t, "admin", *actorRole)

	swappedID, err := recorder.RecordID(ctx, Event{
		ActorID:   42,
		ActorType: ActorTypeUser,
		ActorRole: "admin",
		Action:    "user.updated",
		Resource:  "user",
		RequestID: requestID + "-swapped-actor",
		Source:    SourceHTTP,
		Outcome:   OutcomeStarted,
	})
	require.NoError(t, err)
	err = recorder.Complete(ctx, swappedID, Completion{
		ActorID:    43,
		ActorType:  ActorTypeUser,
		ActorRole:  "admin",
		Outcome:    OutcomeSucceeded,
		StatusCode: 200,
	})
	require.Error(t, err, "a started row must not be completed with a different actor")
	assert.Contains(t, err.Error(), "audit completion may only change completion fields")

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
	// shared test database. TRUNCATE runs as the schema owner: the app role is
	// deliberately not granted it, because TRUNCATE ignores RLS.
	truncateTx, err := owner.Begin(ctx)
	require.NoError(t, err)
	_, err = truncateTx.Exec(ctx, `TRUNCATE audit_events`)
	require.NoError(t, truncateTx.Rollback(ctx))
	require.ErrorContains(t, err, "append-only", "audit_events must reject TRUNCATE")
	var remaining int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE id IN ($1, $2)`, id, serviceID).Scan(&remaining))
	assert.Equal(t, int64(2), remaining)
}

// TestAppRoleCannotTruncate keeps the runtime role's grant list honest:
// TRUNCATE ignores row-level security, so the API/worker role must not have it.
func TestAppRoleCannotTruncate(t *testing.T) {
	pool := pgtest.AppPool(setupAuditTestDB(t))
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `TRUNCATE audit_events`)
	require.NoError(t, tx.Rollback(ctx))
	require.ErrorContains(t, err, "permission denied", "the app role must not be granted TRUNCATE")
}
