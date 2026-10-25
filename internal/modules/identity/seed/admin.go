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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const adminBcryptCost = 12

// Admin creates an admin user if ADMIN_EMAIL and ADMIN_PASSWORD are set
// and no account with that address exists. It is safe to call on every
// startup (idempotent).
//
// The seed only ever creates. When ADMIN_EMAIL already belongs to an account
// that is not an active admin (demoted, banned, or deleted), that state is an
// operator decision: the seed logs a warning and skips. It never re-promotes,
// restores, or fails startup over it. The admin is created with its role and
// verification in one INSERT, so no crash can leave a half-seeded account
// that a later boot would have to repair.
//
// The optional tenant ID selects the tenant that owns the seeded admin. Callers
// pass the configured default tenant; when omitted, the compatibility tenant
// (id 1, slug "default") is used.
func Admin(ctx context.Context, cfg config.AdminConfig, userRepo repository.UserRepository, tenantIDs ...int64) error {
	if cfg.Email == "" || cfg.Password == "" {
		slog.Debug("ADMIN_EMAIL or ADMIN_PASSWORD not set, skipping admin seed")
		return nil
	}

	tenantID := tenant.DefaultID
	if len(tenantIDs) > 0 && tenantIDs[0] > 0 {
		tenantID = tenantIDs[0]
	}
	// The seeder is a trusted system caller: it acts for the platform with no
	// request subject, so mark it for any authz.Authorize on this path.
	ctx = authz.SystemContext(tenant.WithID(ctx, tenantID))

	email := strings.ToLower(strings.TrimSpace(cfg.Email))
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "Admin"
	}

	existing, err := userRepo.GetByEmail(ctx, email)
	if err == nil {
		return keepExisting(ctx, userRepo, existing, email, tenantID)
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return fmt.Errorf("look up admin user: %w", err)
	}
	removed, err := userRepo.GetDeletedByEmail(ctx, email)
	if err == nil {
		slog.Warn("ADMIN_EMAIL belongs to a banned or deleted account; skipping admin seed",
			slog.String("email", email),
			slog.Int64("tenant_id", tenantID),
			slog.Bool("banned", removed.BannedAt.Valid),
		)
		return nil
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return fmt.Errorf("check deleted admin email: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Password), adminBcryptCost)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}
	if _, err := userRepo.CreateAdmin(ctx, identitysqlc.CreateAdminUserParams{
		TenantID:     tenantID,
		Email:        email,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         name,
	}); err != nil {
		// Another replica may have won the startup race; its row decides.
		if database.IsUniqueViolation(err) {
			if winner, lookupErr := userRepo.GetByEmail(ctx, email); lookupErr == nil {
				return keepExisting(ctx, userRepo, winner, email, tenantID)
			}
		}
		return fmt.Errorf("create admin user: %w", err)
	}

	slog.Info("admin user created", slog.String("email", email), slog.Int64("tenant_id", tenantID))
	return nil
}

// keepExisting leaves an active account untouched unless it is already an
// admin whose email is unverified (a pre-hardening seed), which is repaired.
func keepExisting(ctx context.Context, userRepo repository.UserRepository, user *identitysqlc.User, email string, tenantID int64) error {
	if user.Role != dto.RoleAdmin {
		slog.Warn("ADMIN_EMAIL belongs to a non-admin account; skipping admin seed (accounts are never promoted)",
			slog.String("email", email),
			slog.Int64("tenant_id", tenantID),
		)
		return nil
	}
	if !user.EmailVerifiedAt.Valid {
		if _, err := userRepo.VerifyEmail(ctx, user.ID, user.Email); err != nil {
			return fmt.Errorf("mark seeded admin email verified: %w", err)
		}
	}
	slog.Debug("admin user already exists, skipping seed", slog.String("email", email))
	return nil
}
