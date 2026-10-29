//go:build integration

package audit

import (
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// setupAuditTestDB gives each test its own migrated schema and a pool
// connected as the non-superuser app role, so the recorder runs with RLS
// enforced. Dropping the schema removes the rows; audit_events rejects DELETE.
func setupAuditTestDB(t *testing.T) *pgtest.Fixture {
	t.Helper()
	return setupAuditTestDBWithOwner(t)
}

// setupAuditTestDBWithOwner also returns a pool connected as the schema owner.
// A test that must reach a trigger on a statement the app role no longer has
// (TRUNCATE) uses it; application assertions stay on the app fx.Bypass.
func setupAuditTestDBWithOwner(t *testing.T) *pgtest.Fixture {
	t.Helper()
	return pgtest.New(t)
}

// TestPostgresRecorderIntegration inserts an event and reads it back.
func TestPostgresRecorderIntegration(t *testing.T) {
	fx := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-test-%d", time.Now().UnixNano())

	recorder := NewPostgresRecorder(fx.Scoped())
	wantMetadata := map[string]any{
		"reason": "admin-request",
		"admin":  true,
	}
	wantCreatedAt := time.Now().UTC().Truncate(time.Microsecond)

	err := recorder.Record(ctx, Event{
		ActorID:    testID(42),
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
		tenantID   uuid.UUID
		actorID    *uuid.UUID
		action     string
		resource   string
		resourceID *string
		metadata   []byte
		ip         *string
		userAgent  *string
		gotRequest *string
		createdAt  time.Time
	)
	require.NoError(t, fx.Bypass.QueryRow(ctx, `
		SELECT tenant_id, actor_id, action, resource, resource_id, metadata, host(ip), user_agent, request_id, created_at
		FROM audit_events
		WHERE request_id = $1`, requestID,
	).Scan(&tenantID, &actorID, &action, &resource, &resourceID, &metadata, &ip, &userAgent, &gotRequest, &createdAt))

	assert.Equal(t, tenant.DefaultID, tenantID)
	require.NotNil(t, actorID)
	assert.Equal(t, testID(42), *actorID)
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
	fx := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-hostile-%d", time.Now().UnixNano())

	recorder := NewPostgresRecorder(fx.Scoped())
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
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT user_agent, resource_id, metadata FROM audit_events WHERE request_id = $1`, requestID,
	).Scan(&userAgent, &resourceID, &metadata))
	assert.Equal(t, "bad�agent", userAgent)
	assert.Equal(t, "id�", resourceID)
	assert.JSONEq(t, `{"note":"nulhere"}`, string(metadata))
}

// TestPostgresRecorderIntegrationTenant writes tenant-scoped rows and verifies
// that the recorder resolves a missing Event.TenantID from the context.
func TestPostgresRecorderIntegrationTenant(t *testing.T) {
	fx := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	slug := fmt.Sprintf("audit-tenant-%d", time.Now().UnixNano())
	var tenantID uuid.UUID
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, 'Audit Tenant') RETURNING id`, slug).
		Scan(&tenantID))

	requestID := fmt.Sprintf("audit-tenant-test-%d", time.Now().UnixNano())
	ctxRequestID := requestID + "-ctx"
	ctxTenant := tenant.WithID(ctx, tenantID)

	recorder := NewPostgresRecorder(fx.Scoped())
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

	var gotTenant uuid.UUID
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT tenant_id FROM audit_events WHERE request_id = $1`, requestID).Scan(&gotTenant))
	assert.Equal(t, tenantID, gotTenant, "an explicit Event.TenantID must be stored")

	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT tenant_id FROM audit_events WHERE request_id = $1`, ctxRequestID).Scan(&gotTenant))
	assert.Equal(t, tenantID, gotTenant,
		"a zero Event.TenantID must be resolved from the recording context")
}

// TestPostgresRecorderIntegrationDefaults covers the nullable columns and the
// default metadata/created_at handling against a real database.
func TestPostgresRecorderIntegrationDefaults(t *testing.T) {
	fx := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-test-defaults-%d", time.Now().UnixNano())

	before := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, NewPostgresRecorder(fx.Scoped()).Record(ctx, Event{
		Action:    "system.cleanup",
		Resource:  "job",
		RequestID: requestID,
	}))

	var (
		tenantID   uuid.UUID
		actorID    *uuid.UUID
		resourceID *string
		ip         *string
		userAgent  *string
		metadata   []byte
		createdAt  time.Time
	)
	require.NoError(t, fx.Bypass.QueryRow(ctx, `
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
	fx := setupAuditTestDB(t)
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-tx-rollback-%d", time.Now().UnixNano())
	recorder := NewPostgresRecorder(fx.Scoped())
	rolledBack := errors.New("roll back")
	err := fx.Tx().WithTx(ctx, func(tx pgx.Tx) error {
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
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE request_id = $1`, requestID).Scan(&count))
	assert.Zero(t, count, "a rolled-back transaction must leave no audit row")
}

// TestPostgresRecorderCompleteOnce covers intent creation, completion fields,
// one-shot semantics, and the completion-only trigger.
func TestPostgresRecorderCompleteOnce(t *testing.T) {
	fx := setupAuditTestDBWithOwner(t)
	owner := fx.Owner
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("audit-complete-%d", time.Now().UnixNano())

	recorder := NewPostgresRecorder(fx.Scoped())
	intent, err := recorder.RecordID(ctx, Event{
		ActorType:  ActorTypeAnonymous,
		Action:     "auth.login",
		Resource:   "auth",
		RequestID:  requestID,
		Source:     SourceHTTP,
		Outcome:    OutcomeStarted,
		StatusCode: 0,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, intent.ID)
	require.False(t, intent.CreatedAt.IsZero())

	var (
		outcome     string
		completedAt *time.Time
	)
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT outcome, completed_at FROM audit_events WHERE id = $1`, intent.ID).
		Scan(&outcome, &completedAt))
	assert.Equal(t, OutcomeStarted, outcome)
	assert.Nil(t, completedAt)

	require.NoError(t, recorder.Complete(ctx, intent, Completion{
		Outcome:    OutcomeSucceeded,
		StatusCode: 200,
	}))

	var (
		actorID    *uuid.UUID
		actorType  string
		actorRole  *string
		statusCode *int16
	)
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT outcome, completed_at, actor_id, actor_type, actor_role, status_code
		 FROM audit_events WHERE id = $1`, intent.ID).
		Scan(&outcome, &completedAt, &actorID, &actorType, &actorRole, &statusCode))
	assert.Equal(t, OutcomeSucceeded, outcome)
	require.NotNil(t, completedAt)
	assert.Nil(t, actorID, "completion must not invent an actor")
	assert.Equal(t, ActorTypeAnonymous, actorType, "completion must not change the intent actor")
	assert.Nil(t, actorRole)
	require.NotNil(t, statusCode)
	assert.Equal(t, int16(200), *statusCode)

	err = recorder.Complete(ctx, intent, Completion{Outcome: OutcomeFailed, StatusCode: 500})
	require.ErrorIs(t, err, ErrAlreadyCompleted, "completion must happen at most once")

	_, err = fx.Bypass.Exec(ctx,
		`UPDATE audit_events SET action = 'tampered' WHERE id = $1`, intent.ID)
	require.Error(t, err, "completed rows must reject non-completion updates")

	// The actor belongs to the intent: completion keeps it, and an UPDATE that
	// completes the row as another actor is refused by the trigger.
	sameActorIntent, err := recorder.RecordID(ctx, Event{
		ActorID:   testID(42),
		ActorType: ActorTypeUser,
		ActorRole: "admin",
		Action:    "user.updated",
		Resource:  "user",
		RequestID: requestID + "-same-actor",
		Source:    SourceHTTP,
		Outcome:   OutcomeStarted,
	})
	require.NoError(t, err)
	require.NoError(t, recorder.Complete(ctx, sameActorIntent, Completion{
		Outcome:    OutcomeSucceeded,
		StatusCode: 200,
	}))
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT actor_id, actor_type, actor_role FROM audit_events WHERE id = $1`, sameActorIntent.ID).
		Scan(&actorID, &actorType, &actorRole))
	require.NotNil(t, actorID)
	assert.Equal(t, testID(42), *actorID)
	assert.Equal(t, ActorTypeUser, actorType)
	require.NotNil(t, actorRole)
	assert.Equal(t, "admin", *actorRole)

	swappedIntent, err := recorder.RecordID(ctx, Event{
		ActorID:   testID(42),
		ActorType: ActorTypeUser,
		ActorRole: "admin",
		Action:    "user.updated",
		Resource:  "user",
		RequestID: requestID + "-swapped-actor",
		Source:    SourceHTTP,
		Outcome:   OutcomeStarted,
	})
	require.NoError(t, err)
	_, err = fx.Bypass.Exec(ctx, `UPDATE audit_events
		SET outcome = 'succeeded', completed_at = now(), actor_id = $2
		WHERE id = $1`, swappedIntent.ID, testID(43))
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
	var serviceIntent IntentRef
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT id, created_at FROM audit_events WHERE request_id = $1`, requestID+"-service").
		Scan(&serviceIntent.ID, &serviceIntent.CreatedAt))
	err = recorder.Complete(ctx, serviceIntent, Completion{Outcome: OutcomeFailed, StatusCode: 500})
	require.ErrorIs(t, err, ErrAlreadyCompleted)

	_, err = fx.Bypass.Exec(ctx,
		`UPDATE audit_events SET action = 'tampered' WHERE id = $1`, serviceIntent.ID)
	require.Error(t, err, "terminal rows must reject updates")

	// Append-only covers removal too: neither DELETE nor TRUNCATE may drop
	// audit rows outside a reviewed, trigger-disabling maintenance path.
	_, err = fx.Bypass.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, serviceIntent.ID)
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
	require.NoError(t, fx.Bypass.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_events WHERE id IN ($1, $2)`, intent.ID, serviceIntent.ID).Scan(&remaining))
	assert.Equal(t, int64(2), remaining)
}

// TestAppRoleCannotTruncate keeps the runtime role's grant list honest:
// TRUNCATE ignores row-level security, so the API/worker role must not have it.
func TestAppRoleCannotTruncate(t *testing.T) {
	pool := setupAuditTestDB(t).App
	ctx, cancel := context.WithTimeout(tenant.WithID(context.Background(), tenant.DefaultID), 15*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `TRUNCATE audit_events`)
	require.NoError(t, tx.Rollback(ctx))
	require.ErrorContains(t, err, "permission denied", "the app role must not be granted TRUNCATE")
}
