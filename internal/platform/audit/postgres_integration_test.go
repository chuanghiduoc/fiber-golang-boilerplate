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
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_events WHERE request_id = $1`, requestID)
	})

	recorder := NewPostgresRecorder(pool)
	wantMetadata := map[string]any{
		"email": "user@example.com",
		"admin": true,
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
		SELECT actor_id, action, resource, resource_id, metadata, ip, user_agent, request_id, created_at
		FROM audit_events
		WHERE request_id = $1`, requestID,
	).Scan(&actorID, &action, &resource, &resourceID, &metadata, &ip, &userAgent, &gotRequest, &createdAt))

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
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_events WHERE request_id = $1`, requestID)
	})

	before := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, NewPostgresRecorder(pool).Record(ctx, Event{
		Action:    "system.cleanup",
		Resource:  "job",
		RequestID: requestID,
	}))

	var (
		actorID    *int64
		resourceID *string
		ip         *string
		userAgent  *string
		metadata   []byte
		createdAt  time.Time
	)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT actor_id, resource_id, ip, user_agent, metadata, created_at
		FROM audit_events
		WHERE request_id = $1`, requestID,
	).Scan(&actorID, &resourceID, &ip, &userAgent, &metadata, &createdAt))

	assert.Nil(t, actorID)
	assert.Nil(t, resourceID)
	assert.Nil(t, ip)
	assert.Nil(t, userAgent)
	assert.JSONEq(t, `{}`, string(metadata))
	assert.WithinDuration(t, before, createdAt, 2*time.Minute)
}
