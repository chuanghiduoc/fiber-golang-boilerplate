package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// QueryExecModes maps DB_QUERY_EXEC_MODE values onto pgx execution modes.
// config validates the same names. Only modes that describe parameter types
// are supported (see config.DBConfig.QueryExecMode).
var QueryExecModes = map[string]pgx.QueryExecMode{
	"cache_statement": pgx.QueryExecModeCacheStatement,
	"cache_describe":  pgx.QueryExecModeCacheDescribe,
	"describe_exec":   pgx.QueryExecModeDescribeExec,
}

// Pools pairs the normal runtime role pool with the dedicated BYPASSRLS role
// pool. Bypass is nil when the process has no bypass role configured (outside
// production, where OpenPools logs a warning); every cross-tenant path then
// falls back to Primary and only works if that role itself bypasses RLS.
type Pools struct {
	Primary *pgxpool.Pool
	Bypass  *pgxpool.Pool
}

// forBypass returns the pool serving a scope and whether it is the bypass
// role. A nil bypass pool means the process deliberately runs with a single
// role (local/test fallback); the caller must not pretend isolation changed.
func (p Pools) forBypass(bypass bool) (*pgxpool.Pool, bool) {
	if bypass && p.Bypass != nil {
		return p.Bypass, true
	}
	return p.Primary, false
}

// Close closes both pools. Closing the same pool twice is a no-op in pgxpool,
// but the identity check keeps the fallback case explicit.
func (p Pools) Close() {
	if p.Bypass != nil && p.Bypass != p.Primary {
		p.Bypass.Close()
	}
	if p.Primary != nil {
		p.Primary.Close()
	}
}

// OpenPools connects the normal runtime role and, when DB_BYPASS_USERNAME is
// set, the dedicated BYPASSRLS role used for cross-tenant work. Both pools use
// the same host, port, database, schema, TLS settings, pool limits, and
// instrumentation; only the credentials differ.
//
// Outside production the bypass settings are optional: when they are unset the
// process keeps one pool and logs a warning, because global job claim, lease
// recovery, retention purges, catalog sync, and other WithRLSBypass paths then
// only see every tenant if the normal role itself bypasses RLS. Production
// validation requires the pair, so this fallback never happens there.
func OpenPools(ctx context.Context, dbCfg config.DBConfig, opts ...PoolOption) (Pools, error) {
	primary, err := NewPool(ctx, dbCfg, opts...)
	if err != nil {
		return Pools{}, err
	}
	if !dbCfg.BypassConfigured() {
		slog.Warn("DB_BYPASS_USERNAME is not set: cross-tenant paths (job claim, retention, catalog sync) reuse the normal pool and only work if that role bypasses RLS")
		return Pools{Primary: primary}, nil
	}

	bypassCfg := dbCfg
	bypassCfg.Username = dbCfg.BypassUsername
	bypassCfg.Password = dbCfg.BypassPassword
	bypass, err := NewPool(ctx, bypassCfg, opts...)
	if err != nil {
		primary.Close()
		return Pools{}, fmt.Errorf("connect as bypass role: %w", err)
	}
	return Pools{Primary: primary, Bypass: bypass}, nil
}

func NewPool(ctx context.Context, dbCfg config.DBConfig, opts ...PoolOption) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dbCfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to parse pool config: %w", err)
	}
	execMode, ok := QueryExecModes[dbCfg.EffectiveQueryExecMode()]
	if !ok {
		return nil, fmt.Errorf("unsupported DB_QUERY_EXEC_MODE %q", dbCfg.QueryExecMode)
	}
	poolCfg.ConnConfig.DefaultQueryExecMode = execMode

	poolCfg.MaxConns = dbCfg.MaxConns
	poolCfg.MinConns = dbCfg.MinConns
	poolCfg.MaxConnLifetime = dbCfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = dbCfg.MaxConnIdleTime
	if dbCfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = dbCfg.ConnectTimeout
	}
	for _, opt := range opts {
		if opt != nil {
			opt(poolCfg)
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return pool, nil
}

func RunMigrations(dsn, migrationsPath string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("invalid database DSN: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("unsupported database scheme %q", u.Scheme)
	}
	u.Scheme = "pgx5"

	absPath, err := filepath.Abs(migrationsPath)
	if err != nil {
		return fmt.Errorf("resolve migrations path: %w", err)
	}
	sourceURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}).String()

	m, err := migrate.New(sourceURL, u.String())
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	return nil
}

func HealthCheck(ctx context.Context, pool *pgxpool.Pool) map[string]string {
	stats := make(map[string]string)

	if err := pool.Ping(ctx); err != nil {
		stats["status"] = "down"
		stats["error"] = fmt.Sprintf("db down: %v", err)
		return stats
	}

	stats["status"] = "up"
	stats["message"] = "It's healthy"

	poolStats := pool.Stat()
	stats["total_connections"] = strconv.FormatInt(int64(poolStats.TotalConns()), 10)
	stats["acquired_connections"] = strconv.FormatInt(int64(poolStats.AcquiredConns()), 10)
	stats["idle_connections"] = strconv.FormatInt(int64(poolStats.IdleConns()), 10)
	stats["max_connections"] = strconv.FormatInt(int64(poolStats.MaxConns()), 10)

	return stats
}
