//go:build integration

// Package pgtest provisions isolated PostgreSQL schemas for integration tests.
// It imports no business module, so platform packages can use it without an
// import cycle; internal/testutil adds the module-level fixtures on top.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// SetupTestDB runs migrations against an isolated PostgreSQL schema and
// returns the pool plus a cleanup function. By default it starts a disposable
// testcontainer; set TEST_DATABASE_URL to use an existing PostgreSQL server
// (useful on hosts without Docker). The supplied database must allow CREATE
// SCHEMA and CREATE ROLE.
//
// The returned pool connects as the non-superuser app role, so RLS is
// enforced. Its sessions default app.rls_bypass to on for fixtures and
// assertions that use the raw pool: application code always overrides both
// settings per statement (database.Scoped) or transaction (TxManager), and
// TestRLS proves the enforcement.
func SetupTestDB(ctx context.Context) (*pgxpool.Pool, func(), error) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return setupExternalDB(ctx, dsn)
	}

	pgContainer, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		return nil, nil, err
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, err
	}

	if err := database.RunMigrations(connStr, migrationsPath()); err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, err
	}

	pool, err := OpenAppPool(ctx, connStr, "public")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, err
	}

	cleanup := func() {
		pool.Close()
		_ = pgContainer.Terminate(context.Background())
	}

	return pool, cleanup, nil
}

func setupExternalDB(ctx context.Context, dsn string) (*pgxpool.Pool, func(), error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}

	adminCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse TEST_DATABASE_URL pool config: %w", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to TEST_DATABASE_URL: %w", err)
	}

	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		return nil, nil, fmt.Errorf("create test schema: %w", err)
	}
	dropSchema := func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	}

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	schemaDSN := u.String()

	if err := database.RunMigrations(schemaDSN, migrationsPath()); err != nil {
		dropSchema()
		return nil, nil, fmt.Errorf("run migrations in test schema: %w", err)
	}

	pool, err := OpenAppPool(ctx, schemaDSN, schema)
	if err != nil {
		dropSchema()
		return nil, nil, err
	}

	cleanup := func() {
		pool.Close()
		dropSchema()
	}
	return pool, cleanup, nil
}

// migrationsPath resolves the migrations directory relative to this file.
func migrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "migrations")
}
