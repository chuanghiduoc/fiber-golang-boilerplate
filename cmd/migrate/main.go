package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/joho/godotenv/autoload"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
)

// requiredTables lists the schema objects the application assumes exist. The
// check catches a legacy database that reports migration version 1 but predates
// the consolidated baseline, which golang-migrate alone would treat as current.
var requiredTables = []string{
	"tenants",
	"users",
	"refresh_tokens",
	"password_reset_tokens",
	"email_verification_tokens",
	"files",
	"jobs",
	"audit_events",
}

// requiredColumns catches a legacy schema that has the right table names but
// predates the column hardening folded into the consolidated baseline.
var requiredColumns = map[string][]string{
	"tenants":                   {"slug", "status"},
	"users":                     {"tenant_id", "token_version", "password_changed_at", "banned_at"},
	"refresh_tokens":            {"tenant_id", "token_version"},
	"password_reset_tokens":     {"tenant_id"},
	"email_verification_tokens": {"tenant_id", "email"},
	"files":                     {"tenant_id"},
	"jobs":                      {"tenant_id", "trace_parent"},
	"audit_events": {
		"tenant_id", "parent_id", "actor_type", "actor_role", "source", "outcome",
		"status_code", "error_code", "idempotency_key_hash", "completed_at",
	},
}

// requiredUniqueIndexes are the constraints that prevent duplicate live reset
// and verification tokens, duplicate active emails per tenant, and duplicate
// tenant slugs.
var requiredUniqueIndexes = []string{
	"tenants_slug_key",
	"idx_users_tenant_email_lower_active",
	"idx_users_tenant_google_id_active",
	"idx_password_reset_tokens_user_id",
	"idx_email_verification_tokens_user_id",
}

// requiredTenantLeadingIndexes are the query indexes that must start with
// tenant_id. Checking the first key column (not just the index name) catches a
// legacy global index that kept its name after the tenant rewrite.
var requiredTenantLeadingIndexes = map[string][]string{
	"users": {
		"idx_users_tenant_email_lower_active",
		"idx_users_tenant_google_id_active",
		"idx_users_tenant_role_active",
		"idx_users_tenant_created_at_id",
	},
	"refresh_tokens": {
		"idx_refresh_tokens_tenant_user_id",
	},
	"password_reset_tokens": {
		"idx_password_reset_tokens_tenant_user_id",
	},
	"email_verification_tokens": {
		"idx_email_verification_tokens_tenant_user_id",
	},
	"files": {
		"idx_files_tenant_user_id_deleted_at",
		"idx_files_tenant_user_created",
		"idx_files_tenant_created_at_id",
	},
	"jobs": {
		"idx_jobs_tenant_status_run_at",
		"idx_jobs_tenant_kind_status",
	},
	"audit_events": {
		"idx_audit_events_tenant_created",
		"idx_audit_events_tenant_actor_created",
		"idx_audit_events_tenant_action_created",
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

	logger.Setup(os.Getenv("APP_ENV"), os.Getenv("LOG_LEVEL"))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pool, err := database.NewPool(ctx, dbCfg)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 10*time.Second)
	version, dirty, hasHistory, err := currentMigrationVersion(checkCtx, pool)
	cancelCheck()
	if err != nil {
		return err
	}
	if hasHistory && dirty {
		return fmt.Errorf("database migration state is dirty at version %d; repair it manually before retrying", version)
	}
	if err := database.RunMigrations(dbCfg.DSN(), "migrations"); err != nil {
		if hasHistory && version > 1 {
			return fmt.Errorf(
				"run migrations from legacy version %d: %w; restore the historical migration files "+
					"(they must remain source versions for the recorded history) and add an upgrade "+
					"migration, or follow the manual steps in README.md section %q",
				version, err, "Upgrading an existing database",
			)
		}
		return fmt.Errorf("run migrations: %w", err)
	}
	// Use a fresh timeout: a slow migration must not consume the verification
	// budget and turn a successful schema change into a false failure.
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelVerify()
	if err := verifySchema(verifyCtx, pool, dbCfg.Schema); err != nil {
		return err
	}

	slog.Info("migrations completed")
	return nil
}

// currentMigrationVersion reads the recorded golang-migrate version. It lets
// the command turn an opaque "no migration found" error into an actionable
// legacy-upgrade message without blocking the documented path of restoring the
// historical files.
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

// verifySchema fails loudly when the database is missing expected tables. This
// is the safety net for installations whose migration history is older than the
// consolidated baseline: their version may be "current" while the schema is not.
func verifySchema(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	missing := make([]string, 0)

	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = ANY($2)`,
		schema, requiredTables,
	)
	if err != nil {
		return fmt.Errorf("verify schema: %w", err)
	}
	defer rows.Close()

	foundTables := make(map[string]struct{}, len(requiredTables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("verify schema: scan table name: %w", err)
		}
		foundTables[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify schema: %w", err)
	}
	for _, table := range requiredTables {
		if _, ok := foundTables[table]; !ok {
			missing = append(missing, "table "+table)
		}
	}

	columnRows, err := pool.Query(ctx,
		`SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = ANY($2)`,
		schema, requiredTables,
	)
	if err != nil {
		return fmt.Errorf("verify schema columns: %w", err)
	}
	defer columnRows.Close()

	foundColumns := make(map[string]struct{})
	for columnRows.Next() {
		var table, column string
		if err := columnRows.Scan(&table, &column); err != nil {
			return fmt.Errorf("verify schema: scan column: %w", err)
		}
		foundColumns[table+"."+column] = struct{}{}
	}
	if err := columnRows.Err(); err != nil {
		return fmt.Errorf("verify schema columns: %w", err)
	}
	for table, columns := range requiredColumns {
		if _, ok := foundTables[table]; !ok {
			continue // already reported as a missing table
		}
		for _, column := range columns {
			if _, ok := foundColumns[table+"."+column]; !ok {
				missing = append(missing, "column "+table+"."+column)
			}
		}
	}

	indexRows, err := pool.Query(ctx,
		`SELECT c.relname
		 FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = ANY($2) AND i.indisunique`,
		schema, requiredUniqueIndexes,
	)
	if err != nil {
		return fmt.Errorf("verify schema indexes: %w", err)
	}
	defer indexRows.Close()

	foundIndexes := make(map[string]struct{}, len(requiredUniqueIndexes))
	for indexRows.Next() {
		var name string
		if err := indexRows.Scan(&name); err != nil {
			return fmt.Errorf("verify schema: scan index: %w", err)
		}
		foundIndexes[name] = struct{}{}
	}
	if err := indexRows.Err(); err != nil {
		return fmt.Errorf("verify schema indexes: %w", err)
	}
	for _, index := range requiredUniqueIndexes {
		if _, ok := foundIndexes[index]; !ok {
			missing = append(missing, "unique index "+index)
		}
	}

	// Tenant-owned query indexes must lead with tenant_id, not merely exist.
	// A legacy global index with the same name would otherwise pass.
	tenantIndexNames := make([]string, 0, len(requiredTenantLeadingIndexes))
	for _, indexes := range requiredTenantLeadingIndexes {
		tenantIndexNames = append(tenantIndexNames, indexes...)
	}
	leadingRows, err := pool.Query(ctx,
		`SELECT c.relname, a.attname
		 FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid
		 JOIN pg_class t ON t.oid = i.indrelid
		 JOIN pg_namespace n ON n.oid = t.relnamespace
		 JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = i.indkey[0]
		 WHERE n.nspname = $1 AND c.relname = ANY($2)`,
		schema, tenantIndexNames,
	)
	if err != nil {
		return fmt.Errorf("verify tenant-leading indexes: %w", err)
	}
	defer leadingRows.Close()

	foundLeading := make(map[string]string, len(tenantIndexNames))
	for leadingRows.Next() {
		var index, firstColumn string
		if err := leadingRows.Scan(&index, &firstColumn); err != nil {
			return fmt.Errorf("verify schema: scan tenant-leading index: %w", err)
		}
		foundLeading[index] = firstColumn
	}
	if err := leadingRows.Err(); err != nil {
		return fmt.Errorf("verify tenant-leading indexes: %w", err)
	}
	for table, indexes := range requiredTenantLeadingIndexes {
		for _, index := range indexes {
			firstColumn, ok := foundLeading[index]
			switch {
			case !ok:
				missing = append(missing, "tenant-leading index "+table+"."+index)
			case firstColumn != "tenant_id":
				missing = append(missing, "tenant-leading index "+table+"."+index+
					" starts with "+firstColumn+", want tenant_id")
			}
		}
	}

	// The default tenant is the backward-compatibility anchor: rows without an
	// explicit tenant (and legacy requests without X-Tenant-Slug) resolve to it.
	if _, ok := foundTables["tenants"]; ok {
		var tenantID int64
		err := pool.QueryRow(ctx,
			`SELECT id FROM tenants WHERE slug = 'default'`,
		).Scan(&tenantID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			missing = append(missing, "default tenant row (slug default)")
		case err != nil:
			return fmt.Errorf("verify default tenant: %w", err)
		case tenantID != 1:
			missing = append(missing, "default tenant id 1")
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"schema is incomplete (missing: %s); if this database used the previous numbered migrations, see README.md section %q",
			strings.Join(missing, ", "), "Upgrading an existing database",
		)
	}
	return nil
}
