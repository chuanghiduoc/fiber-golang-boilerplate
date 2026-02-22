package seed

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

// Admin creates an admin user if ADMIN_EMAIL and ADMIN_PASSWORD are set
// and the user does not already exist. It is safe to call on every startup (idempotent).
func Admin(ctx context.Context, cfg config.AdminConfig, userRepo repository.UserRepository) error {
	if cfg.Email == "" || cfg.Password == "" {
		slog.Debug("ADMIN_EMAIL or ADMIN_PASSWORD not set, skipping admin seed")
		return nil
	}

	_, err := userRepo.GetByEmail(ctx, cfg.Email)
	if err == nil {
		slog.Debug("admin user already exists, skipping seed", slog.String("email", cfg.Email))
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), 12)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}

	user, err := userRepo.Create(ctx, sqlc.CreateUserParams{
		Email:        cfg.Email,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         cfg.Name,
	})
	if err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}

	if _, err := userRepo.UpdateRole(ctx, sqlc.UpdateUserRoleParams{
		ID:   user.ID,
		Role: dto.RoleAdmin,
	}); err != nil {
		return fmt.Errorf("set admin role: %w", err)
	}

	slog.Info("admin user created", slog.String("email", cfg.Email))
	return nil
}
