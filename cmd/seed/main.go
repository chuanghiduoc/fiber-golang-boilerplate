package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/joho/godotenv/autoload"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/seed"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/logger"
)

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

	ctx := context.Background()
	pool, err := database.NewPool(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if err := database.RunMigrations(cfg.DB.DSN(), "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	userRepo := repository.NewUserRepository(pool)

	if cfg.Admin.Email == "" || cfg.Admin.Password == "" {
		return fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be set for seeding")
	}

	if err := seed.Admin(ctx, cfg.Admin, userRepo); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}

	slog.Info("seed completed")
	return nil
}
