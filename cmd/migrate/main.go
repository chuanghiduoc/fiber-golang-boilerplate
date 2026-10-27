package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/migrate"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
)

const (
	// connectTimeout bounds the initial database connection.
	connectTimeout = 60 * time.Second
	// schemaCheckTimeout bounds each read-only pass (the pre-migration
	// version check and the post-migration verification) separately, so a
	// slow migration cannot consume the verification budget.
	schemaCheckTimeout = 10 * time.Second
)

// requiredTriggers are the database-level invariants the application relies
// on and that the snapshot alone cannot express (enabled state and semantics):
// the per-tenant last-owner and role-scope guards, and the audit
// append-only guards.
func main() {
	writeSnapshot := flag.Bool("write-snapshot", false,
		"write migrations/schema.snapshot.sql from the migrated database instead of verifying it")
	flag.Parse()
	if err := run(*writeSnapshot); err != nil {
		slog.Error("migration failed", slog.Any("error", err))
		os.Exit(1)
	}
}

// run applies all pending up migrations against the configured database.
func run(writeSnapshot bool) error {
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
		return fmt.Errorf("run migrations: %w", err)
	}
	partitionCtx, cancelPartition := context.WithTimeout(context.Background(), schemaCheckTimeout)
	partitionErr := migrate.EnsureAuditPartitions(partitionCtx, pool)
	cancelPartition()
	if partitionErr != nil {
		return partitionErr
	}
	// The connect context is cancelled above; use a fresh one for the revoke
	// so a slow migration cannot turn it into a false failure.
	revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), schemaCheckTimeout)
	revokeErr := revokeMigrationTableAccess(revokeCtx, pool)
	cancelRevoke()
	if revokeErr != nil {
		return revokeErr
	}
	// Use a fresh timeout: a slow migration must not consume the verification
	// budget and turn a successful schema change into a false failure.
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), schemaCheckTimeout)
	defer cancelVerify()
	if writeSnapshot {
		if err := migrate.WriteSchemaSnapshot(verifyCtx, pool, dbCfg.Schema, migrate.DefaultSnapshotPath); err != nil {
			return err
		}
		slog.Info("schema snapshot written", slog.String("path", migrate.DefaultSnapshotPath))
		return nil
	}
	if err := migrate.VerifySchema(verifyCtx, pool, dbCfg.Schema, migrate.DefaultSnapshotPath); err != nil {
		return err
	}

	slog.Info("migrations completed")
	return nil
}

// revokeSchemaMigrationsSQL removes every explicit grant on the migration
// ledger from roles other than its owner. golang-migrate creates
// schema_migrations as the schema owner, so database-roles.sql's default
// privileges hand the runtime roles DML on it; that would let a compromised
// app role rewrite the recorded version (replaying or skipping DDL).
//
// The statement is constant: the dynamic part lives inside the DO block, so a
// role name from the catalog is always quoted with format(%I). It runs after
// every migration, which also covers deployments whose role script ran before
// the table existed.
const revokeSchemaMigrationsSQL = `
DO $$
DECLARE
    target REGCLASS := to_regclass('schema_migrations');
    grantee OID;
BEGIN
    IF target IS NULL THEN
        RETURN;
    END IF;
    FOR grantee IN
        SELECT DISTINCT a.grantee
        FROM pg_class c
        CROSS JOIN LATERAL aclexplode(c.relacl) a
        WHERE c.oid = target
          AND a.grantee <> c.relowner
          AND a.grantee <> 0
    LOOP
        EXECUTE format('REVOKE ALL ON TABLE %s FROM %I', target, grantee::regrole);
    END LOOP;
END;
$$`

// revokeMigrationTableAccess applies revokeSchemaMigrationsSQL. Only the
// schema owner keeps privileges on schema_migrations after a successful run.
func revokeMigrationTableAccess(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, revokeSchemaMigrationsSQL); err != nil {
		return fmt.Errorf("revoke runtime access to schema_migrations: %w", err)
	}
	return nil
}

// currentMigrationVersion reads the recorded golang-migrate version so a dirty
// state can be reported clearly before running. It only reads the version: the
// command never rewrites schema_migrations itself.
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
