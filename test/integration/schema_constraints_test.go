//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// requireFKViolation accepts both SQLSTATEs PostgreSQL uses for FK failures:
// 23503, and 23001 for ON DELETE RESTRICT on PostgreSQL 18+.
func requireFKViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	got, ok := database.ForeignKeyViolation(err)
	require.True(t, ok, "expected a foreign key violation, got %v", err)
	assert.Equal(t, constraint, got)
}

func schemaInsertID(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&id))
	return id
}

// TestSchemaConstraints exercises the baseline invariants that the
// application relies on but never checks itself: identity keys, tenant-
// composite foreign keys, delete actions, and updated_at triggers.
func TestSchemaConstraints(t *testing.T) {
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx := context.Background()

	t.Run("tenant identity starts after the seeded default tenant", func(t *testing.T) {
		id := schemaInsertID(t, pool, `INSERT INTO tenants (slug, name) VALUES ('schema-next', 'Next') RETURNING id`)
		assert.Greater(t, id, int64(1))

		_, err := pool.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES (999, 'schema-explicit', 'Explicit')`)
		require.Error(t, err, "GENERATED ALWAYS must reject an explicit id")
	})

	tenantB := schemaInsertID(t, pool, `INSERT INTO tenants (slug, name) VALUES ('schema-b', 'B') RETURNING id`)
	userA := schemaInsertID(t, pool,
		`INSERT INTO users (tenant_id, email, name) VALUES (1, 'schema-a@example.com', 'A') RETURNING id`)

	t.Run("cross-module children must belong to the user's tenant", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
			 VALUES ($1, $2, 'x', 'schema/cross', 'text/plain', 1)`, tenantB, userA)
		requireFKViolation(t, err, "fk_files_tenant_user")

		_, err = pool.Exec(ctx,
			`INSERT INTO notifications (tenant_id, user_id, type, title, body) VALUES ($1, $2, 't', 't', 'b')`,
			tenantB, userA)
		requireFKViolation(t, err, "fk_notifications_tenant_user")

		_, err = pool.Exec(ctx,
			`INSERT INTO notification_preferences (tenant_id, user_id, type, channel) VALUES ($1, $2, 't', 'email')`,
			tenantB, userA)
		requireFKViolation(t, err, "fk_notification_preferences_tenant_user")
	})

	fileID := schemaInsertID(t, pool,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES (1, $1, 'x', 'schema/own', 'text/plain', 1) RETURNING id`, userA)

	t.Run("hard-deleting a user with files is restricted", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userA)
		requireFKViolation(t, err, "fk_files_tenant_user")
	})

	t.Run("files and notifications maintain updated_at", func(t *testing.T) {
		var before, after time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT updated_at FROM files WHERE id = $1`, fileID).Scan(&before))
		require.NoError(t, pool.QueryRow(ctx,
			`UPDATE files SET deleted_at = NOW() WHERE id = $1 RETURNING updated_at`, fileID).Scan(&after))
		assert.True(t, after.After(before), "files.updated_at must advance on soft delete")

		notificationID := schemaInsertID(t, pool,
			`INSERT INTO notifications (tenant_id, user_id, type, title, body) VALUES (1, $1, 't', 't', 'b') RETURNING id`,
			userA)
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT updated_at FROM notifications WHERE id = $1`, notificationID).Scan(&before))
		require.NoError(t, pool.QueryRow(ctx,
			`UPDATE notifications SET read_at = NOW() WHERE id = $1 RETURNING updated_at`, notificationID).Scan(&after))
		assert.True(t, after.After(before), "notifications.updated_at must advance on read")
	})

	endpointA := schemaInsertID(t, pool,
		`INSERT INTO webhook_endpoints (tenant_id, url) VALUES (1, 'https://example.com/hook') RETURNING id`)

	insertDelivery := func(tenantID int64, eventID string) (int64, error) {
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO webhook_deliveries
			     (tenant_id, endpoint_id, event_id, event_type, event_created_at, url, payload,
			      secret_ciphertext, max_attempts)
			 VALUES ($1, $2, $3, 'test.event', NOW(), 'https://example.com/hook', '{}', 'c', 3)
			 RETURNING id`, tenantID, endpointA, eventID).Scan(&id)
		return id, err
	}

	t.Run("webhook deliveries must reference an endpoint in the same tenant", func(t *testing.T) {
		_, err := insertDelivery(tenantB, "evt-cross")
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_endpoint")
	})

	t.Run("endpoint delete is restricted while purging a delivery cascades its attempts", func(t *testing.T) {
		deliveryID, err := insertDelivery(1, "evt-own")
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO webhook_delivery_attempts (tenant_id, delivery_id, attempt) VALUES (1, $1, 1)`, deliveryID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id = $1`, endpointA)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_endpoint")

		_, err = pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id = $1`, deliveryID)
		require.NoError(t, err)
		var attempts int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_delivery_attempts WHERE delivery_id = $1`, deliveryID).Scan(&attempts))
		assert.Zero(t, attempts)
	})
}
