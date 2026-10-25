package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/joho/godotenv/autoload"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
)

// seedTimeout bounds connecting, the tenant lookup, and the admin seed.
const seedTimeout = 60 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger.Setup(cfg.App.Env, cfg.App.LogLevel)

	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()

	pool, err := database.NewPool(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	// Schema management is owned by cmd/migrate; fail fast if it has not run.
	// Seeding reads and writes tenant-owned rows, so it uses the scoped handle
	// like the API.
	stores := repository.NewStores(database.NewScoped(pool, database.WithResolver(app.RLSResolver)))
	// The seed writes no audit rows, so the transactor has no recorder.
	transactor := repository.NewTransactor(
		database.NewTxManager(pool, database.WithResolver(app.RLSResolver)), nil, nil, nil)
	// Seeding checks existence, not request eligibility: an operator may need
	// to seed an admin before reactivating a suspended default tenant.
	_, defaultTenant, err := service.StartupTenants(ctx, stores.Tenants, cfg.Tenant.DefaultSlug)
	if err != nil {
		return err
	}
	if !defaultTenant.Status.Operational() {
		slog.Warn("configured default tenant is not operational; seeding anyway",
			slog.String("tenant", defaultTenant.Slug),
			slog.Int64("tenant_id", defaultTenant.ID),
			slog.String("status", string(defaultTenant.Status)),
		)
	}

	if cfg.Admin.Email == "" || cfg.Admin.Password == "" {
		return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be set for seeding")
	}

	if err := seed.Admin(ctx, cfg.Admin, stores, transactor, defaultTenant.ID); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}

	slog.Info("seed completed")
	return nil
}
