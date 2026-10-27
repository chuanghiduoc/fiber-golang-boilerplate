//go:build integration

// Package pgtest provisions isolated PostgreSQL schemas for integration tests.
// It imports no business module, so platform packages can use it without an
// import cycle; internal/testutil adds the module-level fixtures on top.
//
// A test package starts ONE PostgreSQL container (or connects once to
// TEST_DATABASE_URL) through Main, and every New call creates its own schema
// and pools inside it. Tests own their pools explicitly: there is no global
// pool registry, so Scoped/Tx helpers take the fixture's Pools (or the
// Fixture methods) instead of looking a pool up by pointer.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
)

// Fixture is one test's isolated schema and pools. Bypass is the fixture pool
// (it sees every tenant, so inserts and assertions use it); App is the normal
// role pool application code must run as; Owner is the schema owner for
// owner-only DDL. Pools bundles App and Bypass for the Scoped/Tx helpers.
type Fixture struct {
	Pools  database.Pools
	App    *pgxpool.Pool
	Bypass *pgxpool.Pool
	Owner  *pgxpool.Pool
	Schema string

	admin *pgxpool.Pool
}

// Scoped returns the DBTX the application uses: every statement carries the
// RLS scope resolved from its context.
func (f *Fixture) Scoped() *database.Scoped { return Scoped(f.Pools) }

// Tx returns a transaction manager using the application's fail-closed
// resolver.
func (f *Fixture) Tx() *database.TxManager { return NewTxManager(f.Pools) }

// BypassScoped returns a DBTX for a deliberately cross-tenant store.
func (f *Fixture) BypassScoped() *database.Scoped { return BypassScoped(f.Pools) }

// Close closes this fixture's pools and drops its schema. New registers it with
// t.Cleanup; tests never call it directly except through that cleanup.
func (f *Fixture) Close() {
	ClosePools(f.Pools)
	if f.Owner != nil {
		f.Owner.Close()
	}
	if f.admin != nil {
		_, _ = f.admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+quoteIdent(f.Schema)+" CASCADE")
		f.admin.Close()
	}
}

// shared is the one server (container or external PostgreSQL) a test package
// uses. Main starts it before m.Run; New starts it lazily for packages that do
// not define TestMain (a missing helper is still safe, just less explicit).
var shared struct {
	once    sync.Once
	dsn     string
	admin   *pgxpool.Pool
	cleanup func()
	err     error
}

func startShared(ctx context.Context) error {
	shared.once.Do(func() {
		shared.dsn, shared.cleanup, shared.err = startServer(ctx)
		if shared.err != nil {
			return
		}
		shared.admin, shared.err = openAdminPool(ctx, shared.dsn)
		if shared.err != nil {
			shared.cleanup()
			shared.cleanup = nil
			return
		}
		shared.err = ensureAppRole(ctx, shared.admin)
	})
	return shared.err
}

// startServer returns the admin DSN and a cleanup function. TEST_DATABASE_URL
// reuses an existing server; otherwise one postgres container is started per
// test package.
func startServer(ctx context.Context) (string, func(), error) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn, func() {}, nil
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
		return "", nil, err
	}
	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgContainer.Terminate(ctx)
		return "", nil, err
	}
	return connStr, func() { _ = pgContainer.Terminate(context.Background()) }, nil
}

// New provisions a fresh schema in the package's PostgreSQL server, applies the
// migrations, creates the app and bypass role pools, and registers cleanup.
func New(t testing.TB) *Fixture {
	t.Helper()
	ctx := context.Background()
	if err := startShared(ctx); err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}

	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := shared.admin.Exec(ctx, "CREATE SCHEMA "+quoteIdent(schema)); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	fixture := &Fixture{Schema: schema, admin: openSchemaAdmin(ctx, shared.dsn, schema)}
	t.Cleanup(fixture.Close)

	if err := database.RunMigrations(fixture.schemaDSN(), migrationsPath()); err != nil {
		t.Fatalf("run migrations in test schema: %v", err)
	}

	owner, err := openOwnerPool(ctx, fixture.schemaDSN(), schema)
	if err != nil {
		t.Fatalf("connect as schema owner: %v", err)
	}
	fixture.Owner = owner

	pools, err := OpenAppPools(ctx, fixture.schemaDSN(), schema)
	if err != nil {
		t.Fatalf("open app pools: %v", err)
	}
	fixture.Pools = pools
	fixture.App = pools.Primary
	fixture.Bypass = pools.Bypass
	return fixture
}

// openSchemaAdmin returns a pool for the test's schema so Close can drop it.
// It is separate from the owner pool because dropping the schema must happen
// after every connection that used it is closed.
func openSchemaAdmin(ctx context.Context, dsn, schema string) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil
	}
	return pool
}

func (f *Fixture) schemaDSN() string {
	u, err := url.Parse(shared.dsn)
	if err != nil {
		return shared.dsn
	}
	q := u.Query()
	q.Set("search_path", f.Schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func quoteIdent(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
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

// migrationsPath resolves the migrations directory relative to this file.
func migrationsPath() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "migrations")
}
