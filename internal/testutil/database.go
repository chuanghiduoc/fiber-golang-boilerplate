//go:build integration

package testutil

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

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// SetupTestDB runs migrations against an isolated PostgreSQL schema and
// returns the pool plus a cleanup function. By default it starts a disposable
// testcontainer; set TEST_DATABASE_URL to use an existing PostgreSQL server
// (useful on hosts without Docker). The supplied database must allow CREATE
// SCHEMA.
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

	// Run migrations
	migrationsDir := migrationsPath()
	if err := database.RunMigrations(connStr, migrationsDir); err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, err
	}

	// Create pool from connection string directly
	poolCfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, err
	}
	poolCfg.MaxConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return nil, nil, err
	}

	if err := syncPermissions(ctx, pool); err != nil {
		pool.Close()
		_ = pgContainer.Terminate(context.Background())
		return nil, nil, err
	}

	cleanup := func() {
		pool.Close()
		_ = pgContainer.Terminate(context.Background())
	}

	return pool, cleanup, nil
}

// syncPermissions mirrors the API startup: tests call services and routes
// directly, so the code-owned catalog must reach the database before any
// session loads its snapshot.
func syncPermissions(ctx context.Context, pool *pgxpool.Pool) error {
	stores := identityrepo.NewStores(pool)
	tx := identityrepo.NewTransactor(database.NewTxManager(pool), nil, nil, nil)
	return identityservice.SyncPermissions(ctx, tx, stores.Roles)
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

	poolCfg, err := pgxpool.ParseConfig(schemaDSN)
	if err != nil {
		dropSchema()
		return nil, nil, fmt.Errorf("parse test schema pool config: %w", err)
	}
	poolCfg.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		dropSchema()
		return nil, nil, fmt.Errorf("connect to test schema: %w", err)
	}

	if err := syncPermissions(ctx, pool); err != nil {
		pool.Close()
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
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}
