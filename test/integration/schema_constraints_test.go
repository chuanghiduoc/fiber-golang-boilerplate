//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

	tenantB := testutil.CreateTenant(t, ctx, pool, "schema-b")
	// userA is a global user with a membership in tenant 1 only.
	userA := testutil.CreateMember(t, ctx, pool, 1, "schema-a@example.com", testutil.MemberOptions{}).UserID

	t.Run("users store a normalized email", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO users (email, name) VALUES ('Schema-Mixed@Example.com', 'M')`)
		requireCheckViolation(t, err, "chk_users_email_normalized")
	})

	t.Run("membership state checks and uniqueness", func(t *testing.T) {
		userID := schemaInsertID(t, pool,
			`INSERT INTO users (email, name) VALUES ('schema-states@example.com', 'S') RETURNING id`)
		insert := `INSERT INTO memberships (tenant_id, user_id, status, banned_at, removed_at) VALUES ($1, $2, $3, $4, $5)`
		now := time.Now()

		_, err := pool.Exec(ctx, insert, 1, userID, "suspended", nil, nil)
		requireCheckViolation(t, err, "chk_memberships_banned")
		_, err = pool.Exec(ctx, insert, 1, userID, "active", now, nil)
		requireCheckViolation(t, err, "chk_memberships_banned")
		_, err = pool.Exec(ctx, insert, 1, userID, "removed", nil, nil)
		requireCheckViolation(t, err, "chk_memberships_removed")
		_, err = pool.Exec(ctx, insert, 1, userID, "active", nil, now)
		requireCheckViolation(t, err, "chk_memberships_removed")
		_, err = pool.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES (1, $1, 'owner')`, userID)
		requireCheckViolation(t, err, "memberships_role_check")

		_, err = pool.Exec(ctx, insert, 1, userID, "suspended", now, nil)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insert, 1, userID, "active", nil, nil)
		require.True(t, database.IsUniqueViolation(err), "a second membership in one tenant must collide, got %v", err)
		_, err = pool.Exec(ctx, insert, tenantB, userID, "removed", nil, now)
		require.NoError(t, err, "the same user may be a member of another tenant")
	})

	t.Run("tenant-owned rows require a membership of the user in that tenant", func(t *testing.T) {
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

		_, err = pool.Exec(ctx,
			`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash) VALUES ($1, $2, 'k', 'p', 'schema-cross-key')`,
			tenantB, userA)
		requireFKViolation(t, err, "fk_api_keys_tenant_user")

		_, err = pool.Exec(ctx,
			`INSERT INTO sessions (tenant_id, user_id, token_version) VALUES ($1, $2, 0)`, tenantB, userA)
		requireFKViolation(t, err, "fk_sessions_membership")
	})

	fileID := schemaInsertID(t, pool,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES (1, $1, 'x', 'schema/own', 'text/plain', 1) RETURNING id`, userA)

	t.Run("hard-deleting a member with files or a user with memberships is restricted", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = 1 AND user_id = $1`, userA)
		requireFKViolation(t, err, "fk_files_tenant_user")
		_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userA)
		requireFKViolation(t, err, "memberships_user_id_fkey")
	})

	t.Run("the last active admin of a tenant cannot be demoted, banned, or removed", func(t *testing.T) {
		tenantC := testutil.CreateTenant(t, ctx, pool, "schema-admins")
		admin := testutil.CreateMember(t, ctx, pool, tenantC, "schema-admin-c@example.com",
			testutil.MemberOptions{Role: "admin"}).UserID
		// An admin of another tenant must not satisfy tenant C's invariant.
		testutil.CreateMember(t, ctx, pool, tenantB, "schema-admin-b@example.com", testutil.MemberOptions{Role: "admin"})

		where := ` WHERE tenant_id = $1 AND user_id = $2`
		_, err := pool.Exec(ctx, `UPDATE memberships SET role = 'user'`+where, tenantC, admin)
		requireLastAdminViolation(t, err)
		_, err = pool.Exec(ctx, `UPDATE memberships SET status = 'suspended', banned_at = NOW()`+where, tenantC, admin)
		requireLastAdminViolation(t, err)
		_, err = pool.Exec(ctx, `UPDATE memberships SET status = 'removed', removed_at = NOW()`+where, tenantC, admin)
		requireLastAdminViolation(t, err)
		_, err = pool.Exec(ctx, `DELETE FROM memberships`+where, tenantC, admin)
		requireLastAdminViolation(t, err)

		testutil.CreateMember(t, ctx, pool, tenantC, "schema-admin-c2@example.com", testutil.MemberOptions{Role: "admin"})
		_, err = pool.Exec(ctx, `UPDATE memberships SET role = 'user'`+where, tenantC, admin)
		require.NoError(t, err, "a tenant with another active admin may demote one")
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

	// insertDelivery writes the event in the delivery's tenant first, so a
	// failing insert can only fail on the constraint the subtest targets.
	insertDelivery := func(tenantID int64, eventID string) (int64, error) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_events (tenant_id, event_id, event_type, data_version, actor_type, occurred_at, payload)
			 VALUES ($1, $2, 'test.event', 1, 'system', NOW(), '{}') ON CONFLICT DO NOTHING`, tenantID, eventID); err != nil {
			return 0, err
		}
		var id int64
		err := pool.QueryRow(ctx,
			`INSERT INTO webhook_deliveries
			     (tenant_id, endpoint_id, event_id, url, secret_ciphertext, max_attempts)
			 VALUES ($1, $2, $3, 'https://example.com/hook', 'c', 3)
			 RETURNING id`, tenantID, endpointA, eventID).Scan(&id)
		return id, err
	}

	t.Run("webhook deliveries must reference an endpoint in the same tenant", func(t *testing.T) {
		_, err := insertDelivery(tenantB, "evt-cross")
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_endpoint")
	})

	t.Run("webhook deliveries must reference an event in the same tenant", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_deliveries
			     (tenant_id, endpoint_id, event_id, url, secret_ciphertext, max_attempts)
			 VALUES (1, $1, 'evt-cross', 'https://example.com/hook', 'c', 3)`, endpointA)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_event")
	})

	t.Run("webhook subscriptions must reference an endpoint in the same tenant", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_endpoint_subscriptions (tenant_id, endpoint_id, pattern) VALUES ($1, $2, '*')`,
			tenantB, endpointA)
		requireFKViolation(t, err, "fk_webhook_endpoint_subscriptions_tenant_endpoint")
	})

	t.Run("endpoint and event deletes are restricted while purging a delivery cascades its attempts", func(t *testing.T) {
		deliveryID, err := insertDelivery(1, "evt-own")
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO webhook_delivery_attempts (tenant_id, delivery_id, attempt) VALUES (1, $1, 1)`, deliveryID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx,
			`INSERT INTO webhook_delivery_attempts (tenant_id, delivery_id, attempt) VALUES ($1, $2, 2)`,
			tenantB, deliveryID)
		requireFKViolation(t, err, "fk_webhook_delivery_attempts_tenant_delivery")

		_, err = pool.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id = $1`, endpointA)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_endpoint")
		_, err = pool.Exec(ctx, `DELETE FROM webhook_events WHERE tenant_id = 1 AND event_id = 'evt-own'`)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_event")

		_, err = pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id = $1`, deliveryID)
		require.NoError(t, err)
		var attempts int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_delivery_attempts WHERE delivery_id = $1`, deliveryID).Scan(&attempts))
		assert.Zero(t, attempts)
		_, err = pool.Exec(ctx, `DELETE FROM webhook_events WHERE tenant_id = 1 AND event_id = 'evt-own'`)
		require.NoError(t, err, "an unreferenced event can be deleted")
	})

	t.Run("state consistency checks", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO jobs (kind, status, lease_expires_at) VALUES ('schema.check', 'running', NOW() + interval '1 minute')`)
		requireCheckViolation(t, err, "jobs_running_has_owner")
		_, err = pool.Exec(ctx, `INSERT INTO jobs (kind, status, locked_by) VALUES ('schema.check', 'queued', 'w')`)
		requireCheckViolation(t, err, "jobs_running_has_owner")

		_, err = pool.Exec(ctx,
			`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, rotated_at)
			 VALUES (1, $1, 'k', 'p', 'schema-hash-1', NOW())`, userA)
		requireCheckViolation(t, err, "api_keys_rotation_grace_matches_rotated")

		keyID := schemaInsertID(t, pool,
			`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash)
			 VALUES (1, $1, 'k', 'p', 'schema-hash-2') RETURNING id`, userA)
		_, err = pool.Exec(ctx,
			`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'Files Read')`, keyID)
		requireCheckViolation(t, err, "api_key_scopes_permission_key_check")
		_, err = pool.Exec(ctx,
			`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'admin:files:read')`, keyID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx,
			`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
			 VALUES (1, $1, 'x', 'schema/negative', 'text/plain', -1)`, userA)
		requireCheckViolation(t, err, "files_size_check")
	})

	t.Run("one tenant-wide feature flag target per tenant and flag", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO feature_flags (flag_key) VALUES ('schema_flag')`)
		require.NoError(t, err)
		insertTarget := `INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled) VALUES ('schema_flag', 1, $1, TRUE)`
		_, err = pool.Exec(ctx, insertTarget, nil)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insertTarget, nil)
		require.True(t, database.IsUniqueViolation(err), "a second tenant-wide target must collide, got %v", err)
		_, err = pool.Exec(ctx, insertTarget, userA)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insertTarget, userA)
		require.True(t, database.IsUniqueViolation(err), "a second user target must collide, got %v", err)

		_, err = pool.Exec(ctx,
			`INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled) VALUES ('schema_flag', $1, $2, TRUE)`,
			tenantB, userA)
		requireFKViolation(t, err, "feature_flag_targets_user_tenant_fk")
	})

	t.Run("audit events reject admin actor type, non-IP addresses, and dangling parents", func(t *testing.T) {
		insertAudit := `INSERT INTO audit_events (tenant_id, actor_type, action, resource, ip, parent_id)
		                VALUES (1, $1, 'schema.check', 'schema', $2, $3)`
		_, err := pool.Exec(ctx, insertAudit, "admin", nil, nil)
		requireCheckViolation(t, err, "audit_events_actor_type_check")

		_, err = pool.Exec(ctx, insertAudit, "user", "not-an-ip", nil)
		require.Error(t, err, "the INET column must reject a non-address")

		_, err = pool.Exec(ctx, insertAudit, "user", "203.0.113.7", int64(987654321))
		requireFKViolation(t, err, "audit_events_parent_id_fkey")

		parentID := schemaInsertID(t, pool,
			`INSERT INTO audit_events (tenant_id, actor_type, action, resource, ip)
			 VALUES (1, 'user', 'schema.check', 'schema', '2001:db8::1') RETURNING id`)
		_, err = pool.Exec(ctx, insertAudit, "user", "203.0.113.7", parentID)
		require.NoError(t, err)
	})
}

// requireLastAdminViolation asserts err is the protect_last_admin trigger's
// exception (RAISE EXCEPTION, SQLSTATE P0001).
func requireLastAdminViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected the last-admin trigger error, got %v", err)
	assert.Equal(t, "P0001", pgErr.Code, "error: %v", err)
	assert.Equal(t, "cannot remove the last active admin", pgErr.Message)
}

// requireCheckViolation asserts err is a CHECK violation (23514) of constraint.
func requireCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a check violation, got %v", err)
	assert.Equal(t, "23514", pgErr.Code, "error: %v", err)
	assert.Equal(t, constraint, pgErr.ConstraintName)
}
