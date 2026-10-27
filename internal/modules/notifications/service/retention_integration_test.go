//go:build integration

package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	notificationservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

const notificationRetentionDays = 180

func setupRetentionDB(t *testing.T) *pgtest.Fixture {
	t.Helper()
	return pgtest.New(t)
}

// insertNotification creates a notification created createdAgo ago and read
// readAgo ago (unread when readAgo is zero).
func insertNotification(t *testing.T, pool *pgxpool.Pool, tenantID, userID uuid.UUID, createdAgo, readAgo time.Duration) uuid.UUID {
	t.Helper()
	var readAt *time.Time
	if readAgo > 0 {
		ts := time.Now().Add(-readAgo)
		readAt = &ts
	}
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO notifications (tenant_id, user_id, type, title, body, created_at, read_at)
		 VALUES ($1, $2, 't', 't', 'b', $3, $4) RETURNING id`,
		tenantID, userID, time.Now().Add(-createdAgo), readAt).Scan(&id))
	return id
}

func TestRetentionPurgeDeletesOnlyOldReadNotifications(t *testing.T) {
	fx := setupRetentionDB(t)
	pool := fx.Bypass
	tenantB := integrationTenant(t, pool, "retention-b")
	userA := integrationUser(t, pool, tenant.DefaultID, "retention-a@example.com")
	userB := integrationUser(t, pool, tenantB, "retention-b@example.com")

	const day = 24 * time.Hour
	insertNotification(t, pool, tenant.DefaultID, userA, 400*day, 200*day)
	insertNotification(t, pool, tenantB, userB, 300*day, 181*day)
	unreadOld := insertNotification(t, pool, tenant.DefaultID, userA, 400*day, 0)
	readRecently := insertNotification(t, pool, tenantB, userB, 400*day, 10*day)

	purger := notificationservice.NewRetentionPurger(repository.NewRetentionRepository(fx.Scoped()), notificationRetentionDays)
	ctx := context.Background()
	require.NoError(t, purger.HandleJob(ctx, nil))

	remaining := func() []uuid.UUID {
		rows, err := pool.Query(ctx, `SELECT id FROM notifications ORDER BY id`)
		require.NoError(t, err)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		require.NoError(t, err)
		return ids
	}
	assert.Equal(t, []uuid.UUID{unreadOld, readRecently}, remaining(),
		"old read rows are purged in every tenant; unread rows are kept at any age")

	require.NoError(t, purger.HandleJob(ctx, nil), "redelivery is a no-op")
	assert.Equal(t, []uuid.UUID{unreadOld, readRecently}, remaining())
}

// TestRetentionPurgeQueryUsesPartialIndex checks the purge plan on a realistic
// table (100k rows, mostly unread or recently read) instead of an empty one.
func TestRetentionPurgeQueryUsesPartialIndex(t *testing.T) {
	fx := setupRetentionDB(t)
	pool := fx.Bypass
	ctx := context.Background()
	userA := integrationUser(t, pool, tenant.DefaultID, "retention-plan@example.com")
	_, err := pool.Exec(ctx, `
		INSERT INTO notifications (tenant_id, user_id, type, title, body, created_at, read_at)
		SELECT $2, $1, 't', 't', 'b', now() - make_interval(days => 400),
		       CASE
		           WHEN g % 100 = 0 THEN now() - make_interval(days => 200)
		           WHEN g % 3 = 0 THEN now() - make_interval(days => g % 90)
		       END
		FROM generate_series(1, 100000) AS g`, userA, tenant.DefaultID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE notifications`)
	require.NoError(t, err)

	// The batch CTE of PurgeReadNotifications (queries/retention.sql).
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS)
		WITH batch AS (
		    SELECT id FROM notifications
		    WHERE read_at IS NOT NULL
		      AND read_at < now() - make_interval(days => 180)
		    LIMIT 1000
		    FOR UPDATE SKIP LOCKED
		)
		DELETE FROM notifications AS n
		USING batch
		WHERE n.id = batch.id`)
	require.NoError(t, err)
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	plan := strings.Join(lines, "\n")
	t.Log("\n" + plan)
	assert.Contains(t, plan, "idx_notifications_read_at")
}
