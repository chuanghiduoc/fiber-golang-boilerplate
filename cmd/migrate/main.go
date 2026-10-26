package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/joho/godotenv/autoload"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
)

// maxSequentialVersion is the largest pre-timestamp (6-digit -seq) version.
// Every migration, including the init, uses a UTC
// timestamp version, so a database recorded at 1..999999 is a legacy
// sequential history (the old 000001 baseline or 000001-000003) whose source
// files no longer exist in this repository.
const maxSequentialVersion = 999999

const (
	// connectTimeout bounds the initial database connection.
	connectTimeout = 60 * time.Second
	// schemaCheckTimeout bounds each read-only pass (the pre-migration
	// version check and the post-migration verification) separately, so a
	// slow migration cannot consume the verification budget.
	schemaCheckTimeout = 10 * time.Second
)

// requiredTables lists the schema objects the application assumes exist. The
// check catches a database whose recorded version is current but whose schema
// predates the init (for example one baselined by hand), which
// golang-migrate alone would treat as current.
var requiredTables = []string{
	"tenants",
	"tenant_settings",
	"users",
	"user_passwords",
	"user_identities",
	"memberships",
	"permissions",
	"roles",
	"role_permissions",
	"membership_roles",
	"platform_role_assignments",
	"invitations",
	"sessions",
	"refresh_tokens",
	"password_reset_tokens",
	"email_verification_tokens",
	"mfa_factors",
	"mfa_recovery_codes",
	"data_exports",
	"api_keys",
	"api_key_scopes",
	"files",
	"feature_flags",
	"feature_flag_targets",
	"notifications",
	"notification_preferences",
	"jobs",
	"scheduler_leases",
	"scheduler_runs",
	"audit_events",
	"webhook_endpoints",
	"webhook_endpoint_subscriptions",
	"webhook_endpoint_secrets",
	"webhook_events",
	"webhook_deliveries",
	"webhook_delivery_attempts",
	"webhook_inbound_sources",
	"webhook_inbound_events",
}

// requiredColumns catches a legacy schema that has the right table names but
// predates the column hardening folded into the baseline.
var requiredColumns = map[string][]string{
	"tenants":         {"slug", "status", "status_changed_at", "trial_ends_at", "deletion_scheduled_at"},
	"tenant_settings": {"tenant_id", "locale", "timezone"},
	// Global users: no tenant_id, role, or credential columns.
	"users":           {"email", "status", "token_version", "credentials_changed_at", "deleted_at"},
	"user_passwords":  {"user_id", "password_hash", "changed_at"},
	"user_identities": {"user_id", "provider", "subject", "email"},
	"memberships": {
		"tenant_id", "user_id", "status", "banned_at", "banned_email_hash", "removed_at", "credentials_revoked_at",
	},
	"permissions":               {"key", "owner"},
	"roles":                     {"tenant_id", "scope", "key", "name"},
	"role_permissions":          {"role_id", "permission_key"},
	"membership_roles":          {"tenant_id", "user_id", "role_id"},
	"platform_role_assignments": {"user_id", "role_id"},
	"invitations": {
		"tenant_id", "email_normalized", "role", "token_hash", "invited_by", "expires_at", "accepted_at", "accepted_by",
	},
	"sessions":                  {"tenant_id", "user_id", "token_version", "expires_at", "amr", "ip", "last_used_at", "revoked_at", "revoked_reason"},
	"refresh_tokens":            {"tenant_id", "session_id", "token_hash", "used_at"},
	"password_reset_tokens":     {"user_id", "token_hash"},
	"email_verification_tokens": {"user_id", "email", "token_hash"},
	"mfa_factors":               {"user_id", "type", "secret_ciphertext", "confirmed_at", "last_used_step"},
	"mfa_recovery_codes":        {"user_id", "code_hash", "used_at"},
	"data_exports":              {"tenant_id", "user_id", "status", "storage_path", "expires_at"},
	"files":                     {"tenant_id", "updated_at", "purge_started_at", "purge_attempts", "purge_retry_after"},
	"api_keys": {
		"tenant_id", "user_id", "key_hash", "expires_at",
		"revoked_at", "rotated_at", "rotation_grace_until", "last_used_at",
	},
	"api_key_scopes":           {"api_key_id", "permission_key"},
	"feature_flags":            {"flag_key", "enabled", "kill_switch", "description"},
	"feature_flag_targets":     {"flag_key", "tenant_id", "user_id", "enabled"},
	"notifications":            {"tenant_id", "user_id", "type", "title", "body", "data", "read_at", "created_at", "updated_at"},
	"notification_preferences": {"tenant_id", "user_id", "type", "channel", "enabled"},
	"jobs":                     {"tenant_id", "trace_parent", "idempotency_key", "lease_expires_at"},
	"scheduler_leases":         {"name", "holder", "expires_at", "heartbeat_at"},
	"scheduler_runs":           {"schedule", "scheduled_for", "status", "job_id"},
	"audit_events": {
		"tenant_id", "parent_id", "actor_type", "actor_role", "source", "outcome",
		"status_code", "error_code", "idempotency_key_hash", "completed_at",
	},
	"webhook_endpoints":              {"tenant_id", "url", "status", "max_attempts", "timeout_ms", "deleted_at"},
	"webhook_endpoint_subscriptions": {"tenant_id", "endpoint_id", "pattern"},
	"webhook_endpoint_secrets":       {"tenant_id", "endpoint_id", "version", "secret_ciphertext", "active", "grace_expires_at"},
	"webhook_events":                 {"tenant_id", "event_id", "event_type", "occurred_at", "payload", "created_at"},
	"webhook_deliveries": {
		"tenant_id", "endpoint_id", "event_id", "url",
		"secret_ciphertext", "status", "attempts", "max_attempts", "timeout_ms", "next_attempt_at", "is_replay",
	},
	"webhook_delivery_attempts": {"tenant_id", "delivery_id", "attempt", "status_code", "duration_ms"},
	"webhook_inbound_sources": {
		"tenant_id", "name", "slug", "secret_ciphertext", "previous_secret_ciphertext",
		"previous_secret_expires_at", "status", "deleted_at",
	},
	"webhook_inbound_events": {"tenant_id", "source_id", "external_event_id", "signature", "payload"},
}

// requiredUniqueIndexes are the constraints that prevent duplicate live reset
// and verification tokens, duplicate live emails, duplicate memberships and
// external identities, duplicate pending invitations, duplicate tenant slugs,
// and duplicate API-key hashes, plus every unique index an ON CONFLICT target
// or composite FK parent relies on (without it the upsert fails at runtime
// with "no unique or exclusion constraint matching the ON CONFLICT
// specification").
var requiredUniqueIndexes = []string{
	"tenants_slug_key",
	"idx_users_email_live",
	"idx_data_exports_active",
	"uq_user_identities_provider_subject",
	"uq_user_identities_user_provider",
	"uq_memberships_tenant_user",
	"uq_roles_tenant_key",
	"permissions_pkey",
	"role_permissions_pkey",
	"membership_roles_pkey",
	"platform_role_assignments_pkey",
	"uq_sessions_tenant_id",
	"idx_invitations_tenant_email_pending",
	"invitations_token_hash_key",
	"refresh_tokens_token_hash_key",
	"password_reset_tokens_user_id_key",
	"password_reset_tokens_token_hash_key",
	"email_verification_tokens_user_id_key",
	"email_verification_tokens_token_hash_key",
	"idx_webhook_inbound_sources_tenant_slug_active",
	"idx_webhook_inbound_events_source_event",
	"idx_api_keys_key_hash",
	"idx_notification_preferences_unique",
	"idx_mfa_factors_user_confirmed_totp",
	"idx_mfa_factors_user_unconfirmed_totp",
	"idx_jobs_tenant_idempotency_key",
	"idx_scheduler_runs_schedule_slot",
	"idx_feature_flag_targets_tenant_flag_user",
	"idx_webhook_endpoint_secrets_active",
	"idx_webhook_endpoint_secrets_endpoint_version",
	"idx_webhook_deliveries_endpoint_event",
	"idx_webhook_deliveries_tenant_id_id",
	"idx_webhook_delivery_attempts_delivery_attempt",
	// Primary keys that are ON CONFLICT targets (or composite FK parents).
	"api_key_scopes_pkey",
	"webhook_endpoint_subscriptions_pkey",
	"webhook_events_pkey",
}

// requiredTriggers are the database-level invariants the application relies
// on: the per-tenant last-owner and role-scope guards, and the audit
// append-only guards.
var requiredTriggers = map[string][]string{
	"memberships":               {"trigger_memberships_last_owner"},
	"membership_roles":          {"trigger_membership_roles_scope", "trigger_protect_last_owner"},
	"platform_role_assignments": {"trigger_platform_role_scope"},
	"role_permissions":          {"trigger_role_permissions_scope"},
	"data_exports":              {"trigger_data_exports_updated_at"},
	"audit_events": {
		"trigger_audit_events_completion",
		"trigger_audit_events_no_delete",
		"trigger_audit_events_no_truncate",
	},
}

// requiredRLS lists every table with a tenant_id column. Row-level security
// must be enabled and forced on each, with the tenant_isolation policy, or a
// query that forgot its tenant predicate could read another tenant's rows.
var requiredRLS = []string{
	"tenant_settings",
	"memberships",
	"membership_roles",
	"roles",
	"invitations",
	"sessions",
	"refresh_tokens",
	"data_exports",
	"api_keys",
	"files",
	"feature_flag_targets",
	"notifications",
	"notification_preferences",
	"jobs",
	"audit_events",
	"webhook_endpoints",
	"webhook_endpoint_subscriptions",
	"webhook_endpoint_secrets",
	"webhook_events",
	"webhook_deliveries",
	"webhook_delivery_attempts",
	"webhook_inbound_sources",
	"webhook_inbound_events",
}

// requiredTenantLeadingIndexes are the query indexes that must start with
// tenant_id. Checking the first key column (not just the index name) catches a
// legacy global index that kept its name after the tenant rewrite.
var requiredTenantLeadingIndexes = map[string][]string{
	// users, user_passwords, user_identities, and the reset/verification
	// tokens are global and have no tenant_id.
	// uq_memberships_tenant_user (tenant_id, user_id) is verified as a
	// required unique index above.
	"memberships": {
		"idx_memberships_tenant_created_at_user",
		"idx_memberships_tenant_suspended",
		"idx_memberships_tenant_banned_email_hash",
	},
	"roles": {
		"idx_roles_tenant_created_at_id",
	},
	"invitations": {
		"idx_invitations_tenant_email_pending",
		"idx_invitations_tenant_created_at_id",
	},
	"sessions": {
		"idx_sessions_tenant_user_created_at",
	},
	"data_exports": {
		"idx_data_exports_tenant_user_created_at_id",
	},
	"refresh_tokens": {
		"idx_refresh_tokens_tenant_session_id",
	},
	"files": {
		"idx_files_tenant_user_created",
		"idx_files_tenant_created_at_id",
	},
	// Claim and purge are global; the only tenant-scoped jobs lookup is the
	// idempotency key.
	"jobs": {
		"idx_jobs_tenant_idempotency_key",
	},
	"audit_events": {
		"idx_audit_events_tenant_created",
		"idx_audit_events_tenant_actor_created",
		"idx_audit_events_tenant_action_created",
	},
	"notifications": {
		"idx_notifications_tenant_user_created_at_id",
		"idx_notifications_tenant_user_unread",
	},
	"notification_preferences": {
		"idx_notification_preferences_unique",
	},
	"api_keys": {
		"idx_api_keys_tenant_user",
		"idx_api_keys_tenant_created_at_id",
	},
	"feature_flag_targets": {
		"idx_feature_flag_targets_tenant_flag_user",
	},
	"webhook_endpoints": {
		"idx_webhook_endpoints_tenant_created_at_id",
		"idx_webhook_endpoints_tenant_status",
	},
	"webhook_endpoint_secrets": {
		"idx_webhook_endpoint_secrets_tenant_endpoint",
	},
	"webhook_deliveries": {
		"idx_webhook_deliveries_tenant_created_at_id",
		"idx_webhook_deliveries_tenant_endpoint_created",
		"idx_webhook_deliveries_tenant_event",
	},
	"webhook_delivery_attempts": {
		"idx_webhook_delivery_attempts_tenant_delivery",
	},
	"webhook_inbound_sources": {
		"idx_webhook_inbound_sources_tenant_created",
	},
	"webhook_inbound_events": {
		"idx_webhook_inbound_events_tenant_source_received",
	},
}

func main() {
	if err := run(); err != nil {
		slog.Error("migration failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run applies all pending up migrations against the configured database.
func run() error {
	// Migrations need only the database configuration. Do not load the full
	// application config: a migration job must not require JWT/admin/SMTP
	// secrets just to apply DDL.
	dbCfg, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load database config: %w", err)
	}
	// golang-migrate holds a session advisory lock for the whole run, so a
	// transaction-mode pooler in front of the API must be bypassed here
	// (DB_MIGRATE_HOST / DB_MIGRATE_PORT).
	dbCfg = dbCfg.ForMigration()

	logger.Setup(os.Getenv("APP_ENV"), os.Getenv("LOG_LEVEL"))

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	pool, err := database.NewPool(ctx, dbCfg)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), schemaCheckTimeout)
	version, dirty, hasHistory, err := currentMigrationVersion(checkCtx, pool)
	cancelCheck()
	if err != nil {
		return err
	}
	if hasHistory && dirty {
		return fmt.Errorf("database migration state is dirty at version %d; repair it manually before retrying", version)
	}
	if err := database.RunMigrations(dbCfg.DSN(), "migrations"); err != nil {
		if hasHistory && version >= 1 && version <= maxSequentialVersion {
			return fmt.Errorf(
				"run migrations from legacy version %d: %w; this repository ships only timestamped "+
					"migrations (the init replaced 000001), so apply the reviewed upgrade DDL manually, "+
					"verify the schema, then baseline it as described in README.md section %q",
				version, err, "Upgrading an existing database",
			)
		}
		return fmt.Errorf("run migrations: %w", err)
	}
	// Use a fresh timeout: a slow migration must not consume the verification
	// budget and turn a successful schema change into a false failure.
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), schemaCheckTimeout)
	defer cancelVerify()
	if err := verifySchema(verifyCtx, pool, dbCfg.Schema); err != nil {
		return err
	}

	slog.Info("migrations completed")
	return nil
}

// currentMigrationVersion reads the recorded golang-migrate version. It lets
// the command turn an opaque "no migration found" error into an actionable
// legacy-upgrade message. It only reads the version: the command never
// rewrites schema_migrations itself.
func currentMigrationVersion(ctx context.Context, pool *pgxpool.Pool) (version int64, dirty, exists bool, err error) {
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, false, false, fmt.Errorf("check migration version: %w", err)
	}
	if !exists {
		return 0, false, false, nil
	}
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty); err != nil {
		return 0, false, true, fmt.Errorf("check migration version: %w", err)
	}
	return version, dirty, true, nil
}

// defaultTenantID and defaultTenantSlug identify the compatibility tenant the
// baseline seeds.
const (
	defaultTenantID   = 1
	defaultTenantSlug = "default"
)

// verifySchema fails loudly when the database is missing expected objects.
// This is the safety net for installations whose migration history is older
// than the baseline: their version may be "current" while the
// schema is not. Each check returns a description of every missing object, so
// one run reports the whole gap rather than the first difference.
func verifySchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	foundTables, missing, err := checkTables(ctx, pool, schema)
	if err != nil {
		return err
	}

	checks := []func() ([]string, error){
		func() ([]string, error) { return checkColumns(ctx, pool, schema, foundTables) },
		func() ([]string, error) { return checkUniqueIndexes(ctx, pool, schema) },
		func() ([]string, error) { return checkTenantLeadingIndexes(ctx, pool, schema) },
		func() ([]string, error) { return checkRLS(ctx, pool, schema) },
		func() ([]string, error) { return checkTriggers(ctx, pool, schema) },
		func() ([]string, error) { return checkDefaultTenant(ctx, pool, foundTables) },
	}
	for _, check := range checks {
		gaps, err := check()
		if err != nil {
			return err
		}
		missing = append(missing, gaps...)
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"schema is incomplete (missing: %s); if this database used the previous numbered migrations, see README.md section %q",
			strings.Join(missing, ", "), "Upgrading an existing database",
		)
	}
	return nil
}

// checkTables returns the required tables that exist and those that do not.
func checkTables(ctx context.Context, pool *pgxpool.Pool, schema string) (found map[string]struct{}, missing []string, err error) {
	found = make(map[string]struct{}, len(requiredTables))
	err = scanRows(ctx, pool, "verify schema", "verify schema: scan table name",
		func(rows pgx.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			found[name] = struct{}{}
			return nil
		},
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = ANY($2)`,
		schema, requiredTables,
	)
	if err != nil {
		return nil, nil, err
	}

	for _, table := range requiredTables {
		if _, ok := found[table]; !ok {
			missing = append(missing, "table "+table)
		}
	}
	return found, missing, nil
}

// checkColumns reports required columns absent from existing tables. A
// missing table is already reported by checkTables, so its columns are not.
func checkColumns(ctx context.Context, pool *pgxpool.Pool, schema string, foundTables map[string]struct{}) ([]string, error) {
	found := make(map[string]struct{})
	err := scanRows(ctx, pool, "verify schema columns", "verify schema: scan column",
		func(rows pgx.Rows) error {
			var table, column string
			if err := rows.Scan(&table, &column); err != nil {
				return err
			}
			found[table+"."+column] = struct{}{}
			return nil
		},
		`SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = ANY($2)`,
		schema, requiredTables,
	)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, table := range sortedKeys(requiredColumns) {
		if _, ok := foundTables[table]; !ok {
			continue
		}
		for _, column := range requiredColumns[table] {
			if _, ok := found[table+"."+column]; !ok {
				missing = append(missing, "column "+table+"."+column)
			}
		}
	}
	return missing, nil
}

// checkUniqueIndexes reports required unique indexes that are absent or not
// unique.
func checkUniqueIndexes(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	found := make(map[string]struct{}, len(requiredUniqueIndexes))
	err := scanRows(ctx, pool, "verify schema indexes", "verify schema: scan index",
		func(rows pgx.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			found[name] = struct{}{}
			return nil
		},
		`SELECT c.relname
		 FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = ANY($2) AND i.indisunique`,
		schema, requiredUniqueIndexes,
	)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, index := range requiredUniqueIndexes {
		if _, ok := found[index]; !ok {
			missing = append(missing, "unique index "+index)
		}
	}
	return missing, nil
}

// checkTenantLeadingIndexes reports tenant-owned query indexes that are
// absent or do not lead with tenant_id. Checking the first key column (not
// just the name) catches a legacy global index that kept its name after the
// tenant rewrite.
func checkTenantLeadingIndexes(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	tables := sortedKeys(requiredTenantLeadingIndexes)
	names := make([]string, 0, len(requiredTenantLeadingIndexes))
	for _, table := range tables {
		names = append(names, requiredTenantLeadingIndexes[table]...)
	}

	firstColumns := make(map[string]string, len(names))
	err := scanRows(ctx, pool, "verify tenant-leading indexes", "verify schema: scan tenant-leading index",
		func(rows pgx.Rows) error {
			var index, firstColumn string
			if err := rows.Scan(&index, &firstColumn); err != nil {
				return err
			}
			firstColumns[index] = firstColumn
			return nil
		},
		`SELECT c.relname, a.attname
		 FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 JOIN pg_class t ON t.oid = i.indrelid
		 JOIN pg_namespace n ON n.oid = t.relnamespace
		 JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = i.indkey[0]
		 WHERE n.nspname = $1 AND c.relname = ANY($2)`,
		schema, names,
	)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, table := range tables {
		for _, index := range requiredTenantLeadingIndexes[table] {
			firstColumn, ok := firstColumns[index]
			switch {
			case !ok:
				missing = append(missing, "tenant-leading index "+table+"."+index)
			case firstColumn != "tenant_id":
				missing = append(missing, "tenant-leading index "+table+"."+index+
					" starts with "+firstColumn+", want tenant_id")
			}
		}
	}
	return missing, nil
}

// checkRLS reports tenant-owned tables where row-level security is not enabled
// and forced, or where the tenant_isolation policy is missing. A missing table
// is already reported by checkTables.
func checkRLS(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	type state struct {
		enabled bool
		forced  bool
		policy  bool
	}
	found := make(map[string]state, len(requiredRLS))
	err := scanRows(ctx, pool, "verify row-level security", "verify schema: scan row-level security",
		func(rows pgx.Rows) error {
			var table string
			var s state
			if err := rows.Scan(&table, &s.enabled, &s.forced, &s.policy); err != nil {
				return err
			}
			found[table] = s
			return nil
		},
		`SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		        EXISTS (SELECT 1 FROM pg_policy p
		                WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation')
		 FROM pg_class c
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = ANY($2)`,
		schema, requiredRLS,
	)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, table := range requiredRLS {
		s, ok := found[table]
		switch {
		case !ok:
			continue
		case !s.enabled:
			missing = append(missing, "row-level security on "+table)
		case !s.forced:
			missing = append(missing, "forced row-level security on "+table)
		case !s.policy:
			missing = append(missing, "tenant_isolation policy on "+table)
		}
	}
	return missing, nil
}

// checkTriggers reports required triggers that are absent or disabled.
// Triggers enforce invariants the application does not re-check, so a
// disabled trigger is as bad as a missing one.
func checkTriggers(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	tables := sortedKeys(requiredTriggers)
	names := make([]string, 0, len(requiredTriggers))
	for _, table := range tables {
		names = append(names, requiredTriggers[table]...)
	}

	enabled := make(map[string]bool, len(names))
	err := scanRows(ctx, pool, "verify schema triggers", "verify schema: scan trigger",
		func(rows pgx.Rows) error {
			var table, trigger string
			var isEnabled bool
			if err := rows.Scan(&table, &trigger, &isEnabled); err != nil {
				return err
			}
			enabled[table+"."+trigger] = isEnabled
			return nil
		},
		`SELECT c.relname, tg.tgname, tg.tgenabled <> 'D'
		 FROM pg_trigger tg
		 JOIN pg_class c ON c.oid = tg.tgrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND NOT tg.tgisinternal AND tg.tgname = ANY($2)`,
		schema, names,
	)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, table := range tables {
		for _, trigger := range requiredTriggers[table] {
			isEnabled, ok := enabled[table+"."+trigger]
			switch {
			case !ok:
				missing = append(missing, "trigger "+table+"."+trigger)
			case !isEnabled:
				missing = append(missing, "enabled trigger "+table+"."+trigger)
			}
		}
	}
	return missing, nil
}

// checkDefaultTenant reports a missing or renumbered default tenant. It is
// the backward-compatibility anchor: rows without an explicit tenant (and
// legacy requests without X-Tenant-Slug) resolve to it. Without a tenants
// table there is nothing to check; checkTables already reported it.
func checkDefaultTenant(ctx context.Context, pool *pgxpool.Pool, foundTables map[string]struct{}) ([]string, error) {
	if _, ok := foundTables["tenants"]; !ok {
		return nil, nil
	}
	var tenantID int64
	err := pool.QueryRow(ctx, `SELECT id FROM tenants WHERE slug = $1`, defaultTenantSlug).Scan(&tenantID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return []string{"default tenant row (slug default)"}, nil
	case err != nil:
		return nil, fmt.Errorf("verify default tenant: %w", err)
	case tenantID != defaultTenantID:
		return []string{"default tenant id 1"}, nil
	}
	return nil, nil
}

// scanRows runs sql and passes every row to scan. queryErr prefixes query and
// iteration errors and scanErr prefixes scan errors, so each check keeps its
// own operator-facing message.
func scanRows(ctx context.Context, pool *pgxpool.Pool, queryErr, scanErr string,
	scan func(pgx.Rows) error, sql string, args ...any,
) error {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", queryErr, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("%s: %w", scanErr, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w", queryErr, err)
	}
	return nil
}

// sortedKeys returns m's keys in order so the missing-object report is stable
// across runs.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
