package database

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func NewPool(ctx context.Context, dbCfg config.DBConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dbCfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("failed to parse pool config: %w", err)
	}

	poolCfg.MaxConns = dbCfg.MaxConns
	poolCfg.MinConns = dbCfg.MinConns
	poolCfg.MaxConnLifetime = time.Duration(dbCfg.MaxConnLifetime) * time.Second
	poolCfg.MaxConnIdleTime = time.Duration(dbCfg.MaxConnIdleTime) * time.Second

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
	m, err := migrate.New(
		fmt.Sprintf("file://%s", migrationsPath),
		fmt.Sprintf("pgx5://%s", dsn[len("postgres://"):]),
	)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}
	defer func() { srcErr, dbErr := m.Close(); _, _ = srcErr, dbErr }()

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
