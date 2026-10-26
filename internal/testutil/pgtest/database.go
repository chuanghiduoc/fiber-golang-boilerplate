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
// The returned pool connects as the dedicated BYPASSRLS fixture role, so
// fixtures and assertions that use the raw pool see every tenant. Application
// code goes through pgtest.Scoped/NewTxManager, which resolve the normal app
// role pool and keep RLS enforced; TestRLS proves it.
func SetupTestDB(ctx context.Context) (*pgxpool.Pool, func(), error) {
	pool, _, cleanup, err := SetupTestDBWithOwner(ctx)
	return pool, cleanup, err
}

// SetupTestDBWithOwner is SetupTestDB plus a second pool connected as the
// schema owner. Tests that must exercise owner-only DDL (a new table, a
// policy, TRUNCATE past a trigger) use the owner pool; application assertions
// keep using Scoped/AppPool so RLS stays enforced.
func SetupTestDBWithOwner(ctx context.Context) (*pgxpool.Pool, *pgxpool.Pool, func(), error) {
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
		return nil, nil, nil, err
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, nil, err
	}

	if err := database.RunMigrations(connStr, migrationsPath()); err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, nil, err
	}

	owner, err := openOwnerPool(ctx, connStr, "public")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, nil, err
	}

	pools, err := OpenAppPools(ctx, connStr, "public")
	if err != nil {
		owner.Close()
		_ = pgContainer.Terminate(ctx)
		return nil, nil, nil, err
	}

	cleanup := func() {
		ClosePools(pools)
		owner.Close()
		_ = pgContainer.Terminate(context.Background())
	}

	return pools.Bypass, owner, cleanup, nil
}

// openOwnerPool connects as the schema owner with search_path set to schema.
func openOwnerPool(ctx context.Context, dsn, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse owner pool config: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect as schema owner: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping as schema owner: %w", err)
	}
	return pool, nil
}

func setupExternalDB(ctx context.Context, dsn string) (*pgxpool.Pool, *pgxpool.Pool, func(), error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}

	adminCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse TEST_DATABASE_URL pool config: %w", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to TEST_DATABASE_URL: %w", err)
	}

	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		return nil, nil, nil, fmt.Errorf("create test schema: %w", err)
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
		return nil, nil, nil, fmt.Errorf("run migrations in test schema: %w", err)
	}

	owner, err := openOwnerPool(ctx, schemaDSN, schema)
	if err != nil {
		dropSchema()
		return nil, nil, nil, err
	}

	pools, err := OpenAppPools(ctx, schemaDSN, schema)
	if err != nil {
		owner.Close()
		dropSchema()
		return nil, nil, nil, err
	}

	cleanup := func() {
		ClosePools(pools)
		owner.Close()
		dropSchema()
	}
	return pools.Bypass, owner, cleanup, nil
}

// migrationsPath resolves the migrations directory relative to this file.
func migrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "migrations")
}
