package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Admin creates an admin user if ADMIN_EMAIL and ADMIN_PASSWORD are set
// and the user does not already exist. It is safe to call on every startup (idempotent).
func Admin(ctx context.Context, cfg config.AdminConfig, userRepo repository.UserRepository) error {
	if cfg.Email == "" || cfg.Password == "" {
		slog.Debug("ADMIN_EMAIL or ADMIN_PASSWORD not set, skipping admin seed")
		return nil
	}

	email := strings.ToLower(strings.TrimSpace(cfg.Email))
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "Admin"
	}

	verify := func(user *sqlc.User) error {
		if user.EmailVerifiedAt.Valid {
			return nil
		}
		if _, err := userRepo.VerifyEmail(ctx, user.ID, email); err != nil {
			return fmt.Errorf("mark seeded admin email verified: %w", err)
		}
		return nil
	}

	promote := func(existing *sqlc.User) error {
		if existing.Role == dto.RoleAdmin {
			if err := verify(existing); err != nil {
				return err
			}
			slog.Debug("admin user already exists, skipping seed", slog.String("email", email))
			return nil
		}
		// Only repair an account that proves knowledge of the configured admin
		// password. Never promote an arbitrary pre-registered account.
		if !existing.PasswordHash.Valid ||
			bcrypt.CompareHashAndPassword([]byte(existing.PasswordHash.String), []byte(cfg.Password)) != nil {
			return fmt.Errorf("ADMIN_EMAIL %q already belongs to a non-admin account with a different password", email)
		}
		promoted, err := userRepo.UpdateRole(ctx, sqlc.UpdateUserRoleParams{
			ID:   existing.ID,
			Role: dto.RoleAdmin,
		})
		if err != nil {
			return fmt.Errorf("promote existing admin user: %w", err)
		}
		if err := verify(promoted); err != nil {
			return err
		}
		slog.Info("existing user promoted to admin", slog.String("email", email))
		return nil
	}

	existing, err := userRepo.GetByEmail(ctx, email)
	if err == nil {
		return promote(existing)
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return fmt.Errorf("look up admin user: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), 12)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}

	user, err := userRepo.Create(ctx, sqlc.CreateUserParams{
		Email:        email,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         name,
	})
	if err != nil {
		// Another replica may have won the startup race. Treat that as success
		// if the row is now present and promote it.
		if repository.IsUniqueViolation(err) {
			if existing, lookupErr := userRepo.GetByEmail(ctx, email); lookupErr == nil {
				return promote(existing)
			}
		}
		return fmt.Errorf("create admin user: %w", err)
	}

	adminUser, err := userRepo.UpdateRole(ctx, sqlc.UpdateUserRoleParams{
		ID:   user.ID,
		Role: dto.RoleAdmin,
	})
	if err != nil {
		return fmt.Errorf("set admin role: %w", err)
	}
	if err := verify(adminUser); err != nil {
		return err
	}

	slog.Info("admin user created", slog.String("email", email))
	return nil
}
