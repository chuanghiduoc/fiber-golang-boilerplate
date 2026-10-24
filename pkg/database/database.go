package database

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func NewPool(ctx context.Context, dbCfg config.DBConfig, opts ...PoolOption) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dbCfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to parse pool config: %w", err)
	}

	poolCfg.MaxConns = dbCfg.MaxConns
	poolCfg.MinConns = dbCfg.MinConns
	poolCfg.MaxConnLifetime = time.Duration(dbCfg.MaxConnLifetime) * time.Second
	poolCfg.MaxConnIdleTime = time.Duration(dbCfg.MaxConnIdleTime) * time.Second
	if dbCfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = time.Duration(dbCfg.ConnectTimeout) * time.Second
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
