package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const (
	// AppRole is the non-superuser role integration tests run application code
	// as, matching the role the API uses in production. FORCE RLS subjects it
	// to every tenant policy.
	AppRole = "app"
	// BypassRole is the dedicated BYPASSRLS role cross-tenant code runs as. It
	// is not a superuser, owns nothing, and AppRole is not a member of it.
	BypassRole     = "app_bypass"
	appPassword    = "app"
	bypassPassword = "app_bypass"
)

// poolsRegistry maps each pool handed to a test to the pair of runtime pools
// for its schema. SetupTestDB returns the bypass pool as the fixture pool
// (tests insert and assert across tenants with it), while Scoped and
// NewTxManager resolve the normal app pool for tenant-scoped application
// code. Keeping the mapping here avoids threading a second pool through every
// existing test helper.
var poolsRegistry sync.Map

// OpenAppPools creates (or refreshes) both runtime roles, grants them the same
// DML privileges, and returns a pool pair connected as them. adminDSN is the
// privileged DSN that owns the schema.
//
// The returned Bypass pool is the fixture pool: it reads and writes every
// tenant, which fixtures and assertions need. Tests must run application
// assertions through Scoped/NewTxManager (or pools.Primary) so RLS stays
// enforced; a bypass pool proves nothing about row-level security.
func OpenAppPools(ctx context.Context, adminDSN, schema string) (database.Pools, error) {
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return database.Pools{}, fmt.Errorf("parse admin pool config: %w", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return database.Pools{}, fmt.Errorf("connect as schema owner: %w", err)
	}
	defer admin.Close()

	if err := ensureAppRole(ctx, admin); err != nil {
		return database.Pools{}, err
	}
	if err := OpenAppPool(ctx, admin, schema); err != nil {
		return database.Pools{}, err
	}

	appPool, err := openRolePool(ctx, adminDSN, AppRole, appPassword, schema)
	if err != nil {
		return database.Pools{}, err
	}
	bypassPool, err := openRolePool(ctx, adminDSN, BypassRole, bypassPassword, schema)
	if err != nil {
		appPool.Close()
		return database.Pools{}, err
	}
	pools := database.Pools{Primary: appPool, Bypass: bypassPool}
	poolsRegistry.Store(appPool, pools)
	poolsRegistry.Store(bypassPool, pools)
	return pools, nil
}

// AppPool returns the normal role pool paired with pool, or pool itself when
// it was not created by OpenAppPools (a single-role test database).
func AppPool(pool *pgxpool.Pool) *pgxpool.Pool {
	return poolsFor(pool).Primary
}

// Pools returns the normal and bypass pools paired with pool.
func Pools(pool *pgxpool.Pool) database.Pools {
	return poolsFor(pool)
}

func poolsFor(pool *pgxpool.Pool) database.Pools {
	if value, ok := poolsRegistry.Load(pool); ok {
		return value.(database.Pools)
	}
	return database.Pools{Primary: pool}
}

func forgetPools(pools database.Pools) {
	if pools.Primary != nil {
		poolsRegistry.Delete(pools.Primary)
	}
	if pools.Bypass != nil {
		poolsRegistry.Delete(pools.Bypass)
	}
}

// ClosePools forgets the registry entry and closes both pools.
func ClosePools(pools database.Pools) {
	forgetPools(pools)
	pools.Close()
}

func openRolePool(ctx context.Context, adminDSN, role, password, schema string) (*pgxpool.Pool, error) {
	u, err := url.Parse(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parse %s DSN: %w", role, err)
	}
	u.User = url.UserPassword(role, password)
	poolCfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("parse %s pool config: %w", role, err)
	}
	poolCfg.MaxConns = 5
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect as %s role: %w", role, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping as %s role: %w", role, err)
	}
	return pool, nil
}

// appRoleLockKey serializes role creation: roles are cluster-wide, and test
// packages running in parallel otherwise race on the same pg_authid row
// ("tuple concurrently updated").
const appRoleLockKey = 727274

// ensureAppRole creates and configures both runtime roles. The name predates
// the bypass role and is kept because the reviewed dynamic-SQL allowlist keys
// this function.
func ensureAppRole(ctx context.Context, admin *pgxpool.Pool) error {
	conn, err := admin.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, appRoleLockKey); err != nil {
		return fmt.Errorf("lock role setup: %w", err)
	}
	// A failed unlock is harmless: OpenAppPools closes the admin pool right
	// after, which ends the session and releases the lock.
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, appRoleLockKey) }()

	app := pgx.Identifier{AppRole}.Sanitize()
	bypass := pgx.Identifier{BypassRole}.Sanitize()
	if _, err := conn.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+AppRole+`') THEN
			CREATE ROLE `+app+` LOGIN;
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+BypassRole+`') THEN
			CREATE ROLE `+bypass+` LOGIN;
		END IF;
	END $$;`); err != nil {
		return fmt.Errorf("create runtime roles: %w", err)
	}
	if _, err := conn.Exec(ctx,
		`ALTER ROLE `+app+` WITH LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '`+appPassword+`';
		 ALTER ROLE `+bypass+` WITH LOGIN NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '`+bypassPassword+`';
		 REVOKE `+bypass+` FROM `+app); err != nil {
		return fmt.Errorf("configure runtime roles: %w", err)
	}
	return nil
}

// OpenAppPool grants both runtime roles access to the per-test schema. The
// name predates the bypass role and is kept because the reviewed dynamic-SQL
// allowlist keys this function; the schema name is the only runtime value.
func OpenAppPool(ctx context.Context, admin *pgxpool.Pool, schema string) error {
	ident := pgx.Identifier{schema}.Sanitize()
	app := pgx.Identifier{AppRole}.Sanitize()
	bypass := pgx.Identifier{BypassRole}.Sanitize()
	roles := app + ", " + bypass
	statements := []string{
		"GRANT USAGE ON SCHEMA " + ident + " TO " + roles,
		// No TRUNCATE: it ignores row-level security and would let a bug erase
		// every tenant's rows. Tests that must truncate use the owner pool.
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + ident + " TO " + roles,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + ident + " TO " + roles,
		"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA " + ident + " TO " + roles,
		// MAINTAIN lets the fixture role ANALYZE tables so plan-sensitive tests
		// get real statistics. It grants no read/write access and is test-only.
		"GRANT MAINTAIN ON ALL TABLES IN SCHEMA " + ident + " TO " + bypass,
	}
	if _, err := admin.Exec(ctx, strings.Join(statements, ";\n")); err != nil {
		return fmt.Errorf("grant runtime roles: %w", err)
	}
	return nil
}
