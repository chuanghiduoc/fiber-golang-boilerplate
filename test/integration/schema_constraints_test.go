//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// auditActorID is the fixed actor UUID the completion-trigger test writes.
var auditActorID = uuid.MustParse("0192c9b0-0000-7000-8000-00000000002a")

// requireFKViolation accepts both SQLSTATEs PostgreSQL uses for FK failures:
// 23503, and 23001 for ON DELETE RESTRICT on PostgreSQL 18+.
func requireFKViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	got, ok := database.ForeignKeyViolation(err)
	require.True(t, ok, "expected a foreign key violation, got %v", err)
	assert.Equal(t, constraint, got)
}

func schemaInsertID(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&id))
	return id
}

// TestSchemaConstraints exercises the baseline invariants that the
// application relies on but never checks itself: identity keys, tenant-
// composite foreign keys, delete actions, and updated_at triggers.
func TestSchemaConstraints(t *testing.T) {
	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	ctx := context.Background()

	t.Run("tenant ids are generated UUIDv7 values", func(t *testing.T) {
		var defaultTenant uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = 'default'`).Scan(&defaultTenant))
		assert.Equal(t, tenant.DefaultID, defaultTenant, "the seeded default tenant is pinned")
		id := schemaInsertID(t, pool, `INSERT INTO tenants (slug, name) VALUES ('schema-next', 'Next') RETURNING id`)
		assert.Equal(t, uuid.Version(7), id.Version(), "generated ids are UUIDv7")
		assert.NotEqual(t, tenant.DefaultID, id)

	})

	tenantB := testutil.CreateTenant(t, ctx, pool, "schema-b")
	// userA is a global user with a membership in tenant 1 only.
	userA := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, "schema-a@example.com", testutil.MemberOptions{}).UserID

	t.Run("users store a normalized email", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO users (email, name) VALUES ('Schema-Mixed@Example.com', 'M')`)
		requireCheckViolation(t, err, "chk_users_email_normalized")
	})

	t.Run("membership state checks and uniqueness", func(t *testing.T) {
		userID := schemaInsertID(t, pool,
			`INSERT INTO users (email, name) VALUES ('schema-states@example.com', 'S') RETURNING id`)
		insert := `INSERT INTO memberships (tenant_id, user_id, status, banned_at, removed_at) VALUES ($1, $2, $3, $4, $5)`
		now := time.Now()

		_, err := pool.Exec(ctx, insert, tenant.DefaultID, userID, "suspended", nil, nil)
		requireCheckViolation(t, err, "chk_memberships_banned")
		_, err = pool.Exec(ctx, insert, tenant.DefaultID, userID, "active", now, nil)
		requireCheckViolation(t, err, "chk_memberships_banned")
		_, err = pool.Exec(ctx, insert, tenant.DefaultID, userID, "removed", nil, nil)
		requireCheckViolation(t, err, "chk_memberships_removed")
		_, err = pool.Exec(ctx, insert, tenant.DefaultID, userID, "active", nil, now)
		requireCheckViolation(t, err, "chk_memberships_removed")
		_, err = pool.Exec(ctx, insert, tenant.DefaultID, userID, "suspended", now, nil)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insert, tenant.DefaultID, userID, "active", nil, nil)
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
			`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, expires_at)
			 VALUES ($1, $2, 'k', 'p', sha256('schema-cross-key'::bytea), NOW() + interval '1 hour')`,
			tenantB, userA)
		requireFKViolation(t, err, "fk_api_keys_tenant_user")

		_, err = pool.Exec(ctx,
			`INSERT INTO sessions (tenant_id, user_id, token_version, expires_at) VALUES ($1, $2, 0, NOW() + INTERVAL '720 hours')`, tenantB, userA)
		requireFKViolation(t, err, "fk_sessions_membership")
	})

	fileID := schemaInsertID(t, pool,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES ($1, $2, 'x', 'schema/own', 'text/plain', 1) RETURNING id`, tenant.DefaultID, userA)

	t.Run("hard-deleting a member with files or a user with memberships is restricted", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, userA)
		requireFKViolation(t, err, "fk_files_tenant_user")
		_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userA)
		requireFKViolation(t, err, "fk_memberships_user")
	})

	t.Run("the last active owner of a tenant cannot be demoted, banned, or removed", func(t *testing.T) {
		tenantC := testutil.CreateTenant(t, ctx, pool, "schema-owners")
		owner := testutil.CreateMember(t, ctx, pool, tenantC, "schema-owner-c@example.com",
			testutil.MemberOptions{Role: "owner"}).UserID
		// An owner of another tenant must not satisfy tenant C's invariant.
		testutil.CreateMember(t, ctx, pool, tenantB, "schema-owner-b@example.com", testutil.MemberOptions{Role: "owner"})

		where := ` WHERE tenant_id = $1 AND user_id = $2`
		_, err := pool.Exec(ctx, `DELETE FROM membership_roles`+where, tenantC, owner)
		requireLastOwnerViolation(t, err)
		_, err = pool.Exec(ctx, `UPDATE memberships SET status = 'suspended', banned_at = NOW()`+where, tenantC, owner)
		requireLastOwnerViolation(t, err)
		_, err = pool.Exec(ctx, `UPDATE memberships SET status = 'removed', removed_at = NOW()`+where, tenantC, owner)
		requireLastOwnerViolation(t, err)
		_, err = pool.Exec(ctx, `DELETE FROM memberships`+where, tenantC, owner)
		requireLastOwnerViolation(t, err)

		testutil.CreateMember(t, ctx, pool, tenantC, "schema-owner-c2@example.com", testutil.MemberOptions{Role: "owner"})
		_, err = pool.Exec(ctx, `DELETE FROM membership_roles`+where, tenantC, owner)
		require.NoError(t, err, "a tenant with another active owner may remove one")
	})

	t.Run("membership roles must be tenant-scoped and belong to the membership tenant", func(t *testing.T) {
		tenantD := testutil.CreateTenant(t, ctx, pool, "schema-roles")
		tenantRole := schemaInsertID(t, pool,
			`INSERT INTO roles (tenant_id, scope, key, name) VALUES ($1, 'tenant', 'reviewer', 'Reviewer') RETURNING id`,
			tenantD)
		member := testutil.CreateMember(t, ctx, pool, tenantB, "schema-role-b@example.com",
			testutil.MemberOptions{Role: "member"}).UserID

		_, err := pool.Exec(ctx,
			`INSERT INTO roles (tenant_id, scope, key, name) VALUES ($1, 'tenant', 'owner', 'Shadow')`, tenantD)
		requireCheckViolation(t, err, "chk_roles_tenant_key_system")
		_, err = pool.Exec(ctx,
			`INSERT INTO role_permissions (role_id, permission_key) VALUES ($1, 'platform:tenants:read')`, tenantRole)
		requireTriggerViolation(t, err, "tenant roles cannot hold platform permissions")

		_, err = pool.Exec(ctx, `INSERT INTO membership_roles (tenant_id, user_id, role_id) VALUES ($1, $2, $3)`,
			tenantB, member, tenantRole)
		requireTriggerViolation(t, err, "role belongs to another tenant")

		platformRole := schemaInsertID(t, pool,
			`SELECT id FROM roles WHERE tenant_id IS NULL AND key = 'platform_admin'`)
		_, err = pool.Exec(ctx, `INSERT INTO membership_roles (tenant_id, user_id, role_id) VALUES ($1, $2, $3)`,
			tenantB, member, platformRole)
		requireTriggerViolation(t, err, "membership roles must be tenant-scoped")

		_, err = pool.Exec(ctx, `INSERT INTO platform_role_assignments (user_id, role_id) VALUES ($1, $2)`,
			member, tenantRole)
		requireTriggerViolation(t, err, "platform assignments require a platform-scoped role")
	})

	t.Run("files and notifications maintain updated_at", func(t *testing.T) {
		var before, after time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT updated_at FROM files WHERE id = $1`, fileID).Scan(&before))
		require.NoError(t, pool.QueryRow(ctx,
			`UPDATE files SET deleted_at = NOW() WHERE id = $1 RETURNING updated_at`, fileID).Scan(&after))
		assert.True(t, after.After(before), "files.updated_at must advance on soft delete")

		notificationID := schemaInsertID(t, pool,
			`INSERT INTO notifications (tenant_id, user_id, type, title, body) VALUES ($1, $2, 't', 't', 'b') RETURNING id`,
			tenant.DefaultID, userA)
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT updated_at FROM notifications WHERE id = $1`, notificationID).Scan(&before))
		require.NoError(t, pool.QueryRow(ctx,
			`UPDATE notifications SET read_at = NOW() WHERE id = $1 RETURNING updated_at`, notificationID).Scan(&after))
		assert.True(t, after.After(before), "notifications.updated_at must advance on read")
	})

	endpointA := schemaInsertID(t, pool,
		`INSERT INTO webhook_endpoints (tenant_id, url) VALUES ($1, 'https://example.com/hook') RETURNING id`, tenant.DefaultID)
	secretA := schemaInsertID(t, pool,
		`INSERT INTO webhook_endpoint_secrets (tenant_id, endpoint_id, version, secret_ciphertext)
		 VALUES ($1, $2, 1, 'sealed') RETURNING id`, tenant.DefaultID, endpointA)
	crossEventID := uuid.New()
	ownEventID := uuid.New()

	// insertDelivery writes the event in the delivery's tenant first, so a
	// failing insert can only fail on the constraint the subtest targets.
	insertDelivery := func(tenantID uuid.UUID, eventID uuid.UUID) (uuid.UUID, error) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO webhook_events (tenant_id, event_id, event_type, data_version, actor_type, occurred_at, payload)
			 VALUES ($1, $2, 'test.event', 1, 'system', NOW(), '{}') ON CONFLICT DO NOTHING`, tenantID, eventID); err != nil {
			return uuid.Nil, err
		}
		var id uuid.UUID
		err := pool.QueryRow(ctx,
			`INSERT INTO webhook_deliveries
			     (tenant_id, endpoint_id, event_id, secret_id, url, max_attempts)
			 VALUES ($1, $2, $3, $4, 'https://example.com/hook', 3)
			 RETURNING id`, tenantID, endpointA, eventID, secretA).Scan(&id)
		return id, err
	}

	t.Run("webhook deliveries must reference an endpoint in the same tenant", func(t *testing.T) {
		_, err := insertDelivery(tenantB, crossEventID)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_endpoint")
	})

	t.Run("webhook deliveries must reference an event in the same tenant", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_deliveries
			     (tenant_id, endpoint_id, event_id, secret_id, url, max_attempts)
			 VALUES ($1, $2, $3, $4, 'https://example.com/hook', 3)`, tenant.DefaultID, endpointA, crossEventID, secretA)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_event")
	})

	t.Run("webhook subscriptions must reference an endpoint in the same tenant", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO webhook_endpoint_subscriptions (tenant_id, endpoint_id, pattern) VALUES ($1, $2, '*')`,
			tenantB, endpointA)
		requireFKViolation(t, err, "fk_webhook_endpoint_subscriptions_tenant_endpoint")
	})

	t.Run("endpoint and event deletes are restricted while purging a delivery cascades its attempts", func(t *testing.T) {
		deliveryID, err := insertDelivery(tenant.DefaultID, ownEventID)
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`INSERT INTO webhook_delivery_attempts (tenant_id, delivery_id, attempt) VALUES ($1, $2, 1)`, tenant.DefaultID, deliveryID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx,
			`INSERT INTO webhook_delivery_attempts (tenant_id, delivery_id, attempt) VALUES ($1, $2, 2)`,
			tenantB, deliveryID)
		requireFKViolation(t, err, "fk_webhook_delivery_attempts_tenant_delivery")

		_, err = pool.Exec(ctx, `DELETE FROM webhook_endpoints WHERE id = $1`, endpointA)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_endpoint")
		_, err = pool.Exec(ctx, `DELETE FROM webhook_events WHERE tenant_id = $1 AND event_id = $2`, tenant.DefaultID, ownEventID)
		requireFKViolation(t, err, "fk_webhook_deliveries_tenant_event")

		_, err = pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id = $1`, deliveryID)
		require.NoError(t, err)
		var attempts int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_delivery_attempts WHERE delivery_id = $1`, deliveryID).Scan(&attempts))
		assert.Zero(t, attempts)
		_, err = pool.Exec(ctx, `DELETE FROM webhook_events WHERE tenant_id = $1 AND event_id = $2`, tenant.DefaultID, ownEventID)
		require.NoError(t, err, "an unreferenced event can be deleted")
	})

	t.Run("state consistency checks", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO jobs (kind, status, lease_expires_at, tenant_id) VALUES ('schema.check', 'running', NOW() + interval '1 minute', $1)`, tenant.DefaultID)
		requireCheckViolation(t, err, "chk_jobs_running_has_owner")
		_, err = pool.Exec(ctx, `INSERT INTO jobs (kind, status, locked_by, tenant_id) VALUES ('schema.check', 'queued', 'w', $1)`, tenant.DefaultID)
		requireCheckViolation(t, err, "chk_jobs_running_has_owner")

		// The jobs.tenant_id column has no default: a row must name its tenant.
		_, err = pool.Exec(ctx, `INSERT INTO jobs (kind, status) VALUES ('schema.check', 'queued')`)
		requireNotNullViolation(t, err, "tenant_id")

		_, err = pool.Exec(ctx,
			`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, expires_at, rotated_at)
			 VALUES ($1, $2, 'k', 'p', sha256('schema-hash-1'::bytea), NOW() + interval '1 hour', NOW())`, tenant.DefaultID, userA)
		requireCheckViolation(t, err, "chk_api_keys_rotation_grace_matches_rotated")

		keyID := schemaInsertID(t, pool,
			`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, expires_at)
			 VALUES ($1, $2, 'k', 'p', sha256('schema-hash-2'::bytea), NOW() + interval '1 hour') RETURNING id`, tenant.DefaultID, userA)
		_, err = pool.Exec(ctx,
			`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'Files Read')`, keyID)
		requireCheckViolation(t, err, "chk_api_key_scopes_permission_key")
		_, err = pool.Exec(ctx,
			`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'admin:files:read')`, keyID)
		require.NoError(t, err)

		_, err = pool.Exec(ctx,
			`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
			 VALUES ($1, $2, 'x', 'schema/negative', 'text/plain', -1)`, tenant.DefaultID, userA)
		requireCheckViolation(t, err, "files_size_check")
	})

	t.Run("one tenant-wide feature flag target per tenant and flag", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO feature_flags (flag_key) VALUES ('schema_flag')`)
		require.NoError(t, err)
		insertTarget := `INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled) VALUES ('schema_flag', $1, $2, TRUE)`
		_, err = pool.Exec(ctx, insertTarget, tenant.DefaultID, nil)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insertTarget, tenant.DefaultID, nil)
		require.True(t, database.IsUniqueViolation(err), "a second tenant-wide target must collide, got %v", err)
		_, err = pool.Exec(ctx, insertTarget, tenant.DefaultID, userA)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, insertTarget, tenant.DefaultID, userA)
		require.True(t, database.IsUniqueViolation(err), "a second user target must collide, got %v", err)

		_, err = pool.Exec(ctx,
			`INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled) VALUES ('schema_flag', $1, $2, TRUE)`,
			tenantB, userA)
		requireFKViolation(t, err, "fk_feature_flag_targets_user_tenant")
	})

	t.Run("audit events reject admin actor type and non-IP addresses, and keep parent links", func(t *testing.T) {
		insertAudit := `INSERT INTO audit_events (tenant_id, actor_type, action, resource, ip, parent_id)
		                VALUES ($1, $2, 'schema.check', 'schema', $3, $4)`
		_, err := pool.Exec(ctx, insertAudit, tenant.DefaultID, "admin", nil, nil)
		requireCheckViolation(t, err, "chk_audit_events_actor_type")

		_, err = pool.Exec(ctx, insertAudit, tenant.DefaultID, "user", "not-an-ip", nil)
		require.Error(t, err, "the INET column must reject a non-address")

		// audit_events is partitioned, so parent_id has an index but no
		// self-foreign-key: a child may be written before its intent row and a
		// dangling parent is accepted (the old FK was removed with the UUIDv7
		// schema).
		parentID := schemaInsertID(t, pool,
			`INSERT INTO audit_events (tenant_id, actor_type, action, resource, ip)
			 VALUES ($1, 'user', 'schema.check', 'schema', '2001:db8::1') RETURNING id`, tenant.DefaultID)
		childID := schemaInsertID(t, pool, `INSERT INTO audit_events (tenant_id, actor_type, action, resource, ip, parent_id)
			 VALUES ($1, 'user', 'schema.check', 'schema', '203.0.113.7', $2) RETURNING id`, tenant.DefaultID, parentID)
		var linked int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE parent_id = $1 AND id = $2`, parentID, childID).Scan(&linked))
		assert.Equal(t, 1, linked, "the parent link is queryable")
	})

	t.Run("audit completion freezes the intent actor and completion codes", func(t *testing.T) {
		insertIntent := func(statusCode any, errorCode any) uuid.UUID {
			t.Helper()
			return schemaInsertID(t, pool, `
				INSERT INTO audit_events
				    (tenant_id, actor_id, actor_type, actor_role, action, resource,
				     source, outcome, status_code, error_code)
				VALUES ($1, $2, 'user', 'admin', 'schema.complete', 'schema',
				        'http', 'started', $3, $4)
				RETURNING id`, tenant.DefaultID, auditActorID, statusCode, errorCode)
		}

		id := insertIntent(nil, nil)
		for _, update := range []string{
			`actor_id = '0192c9b0-0000-7000-8000-00000000002b'`,
			`actor_type = 'system'`,
			`actor_role = 'owner'`,
		} {
			_, err := pool.Exec(ctx, `
				UPDATE audit_events
				SET outcome = 'succeeded', completed_at = now(), `+update+`
				WHERE id = $1`, id)
			requireTriggerViolation(t, err, "audit completion may only change completion fields")
		}

		// Completing with the same actor is allowed and fills status/error once.
		_, err := pool.Exec(ctx, `
			UPDATE audit_events
			SET outcome = 'succeeded', completed_at = now(), status_code = 200, error_code = 'ok'
			WHERE id = $1`, id)
		require.NoError(t, err)

		codeID := insertIntent(int16(201), nil)
		_, err = pool.Exec(ctx, `
			UPDATE audit_events
			SET outcome = 'succeeded', completed_at = now(), status_code = 500
			WHERE id = $1`, codeID)
		requireTriggerViolation(t, err, "audit completion cannot change status_code")

		errorID := insertIntent(nil, "created")
		_, err = pool.Exec(ctx, `
			UPDATE audit_events
			SET outcome = 'succeeded', completed_at = now(), error_code = 'changed'
			WHERE id = $1`, errorID)
		requireTriggerViolation(t, err, "audit completion cannot change error_code")
	})

	t.Run("a role owned by another tenant cannot receive platform permissions", func(t *testing.T) {
		tenantE := testutil.CreateTenant(t, ctx, pool, "schema-hidden-role")
		hiddenRole := schemaInsertID(t, pool,
			`INSERT INTO roles (tenant_id, scope, key, name) VALUES ($1, 'tenant', 'hidden', 'Hidden') RETURNING id`,
			tenantE)

		app := fx.Scoped()
		// The caller's RLS scope is tenant B, so tenant E's role is invisible.
		_, err := app.Exec(tenant.WithID(ctx, tenantB),
			`INSERT INTO role_permissions (role_id, permission_key) VALUES ($1, 'platform:tenants:read')`, hiddenRole)
		requireTriggerViolation(t, err, "role does not exist or is not visible to the caller")

		// The same grant in the role's own tenant is still refused as a
		// platform permission (the existing rule, not a hidden-row NULL).
		_, err = app.Exec(tenant.WithID(ctx, tenantE),
			`INSERT INTO role_permissions (role_id, permission_key) VALUES ($1, 'platform:tenants:read')`, hiddenRole)
		requireTriggerViolation(t, err, "tenant roles cannot hold platform permissions")
	})

	t.Run("trigger functions pin search_path while the RLS predicate stays inlinable", func(t *testing.T) {
		var schema string
		require.NoError(t, pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema))

		pinned := []string{
			"update_updated_at",
			"validate_role_permission",
			"validate_membership_role",
			"validate_platform_role",
			"protect_last_owner",
			"audit_events_completion_guard",
			"audit_events_append_only",
		}
		rows, err := pool.Query(ctx, `
			SELECT p.proname, p.proconfig
			FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = current_schema()
			  AND p.proname = ANY($1)`, append(pinned, "app_tenant_visible"))
		require.NoError(t, err)
		defer rows.Close()

		seen := 0
		for rows.Next() {
			var (
				name      string
				proconfig []string
			)
			require.NoError(t, rows.Scan(&name, &proconfig))
			seen++
			if name == "app_tenant_visible" {
				assert.Empty(t, proconfig,
					"app_tenant_visible must stay a plain inlinable SQL function without SET")
				continue
			}
			require.Len(t, proconfig, 1, "%s must pin exactly one search_path", name)
			assert.Contains(t, proconfig[0], "pg_catalog", "%s must search pg_catalog first", name)
			assert.Contains(t, proconfig[0], schema, "%s must include the migration schema", name)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, len(pinned)+1, seen, "every trigger function and the RLS predicate must be inspected")
	})
}

// requireLastOwnerViolation asserts err is the protect_last_owner trigger's
// exception (RAISE EXCEPTION, SQLSTATE P0001).
func requireLastOwnerViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected the trigger error, got %v", err)
	assert.Equal(t, "LO001", pgErr.Code, "error: %v", err)
	assert.Equal(t, "cannot remove the last owner", pgErr.Message)
}

func requireTriggerViolation(t *testing.T, err error, message string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected the trigger error, got %v", err)
	assert.Equal(t, "P0001", pgErr.Code, "error: %v", err)
	assert.Equal(t, message, pgErr.Message)
}

// requireCheckViolation asserts err is a CHECK violation (23514) of constraint.
func requireCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a check violation, got %v", err)
	assert.Equal(t, "23514", pgErr.Code, "error: %v", err)
	assert.Equal(t, constraint, pgErr.ConstraintName)
}

// requireNotNullViolation asserts err is a NOT NULL violation (23502) of column.
func requireNotNullViolation(t *testing.T, err error, column string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected a not-null violation, got %v", err)
	assert.Equal(t, "23502", pgErr.Code, "error: %v", err)
	assert.Equal(t, column, pgErr.ColumnName)
}
