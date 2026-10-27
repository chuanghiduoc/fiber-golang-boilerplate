// Package seed creates the optional bootstrap admin.
package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// Admin creates the bootstrap admin when ADMIN_EMAIL and ADMIN_PASSWORD are
// set and no account has that address: a verified global user, its password,
// an owner membership in the tenant, and the platform_admin assignment, in one
// transaction. It is idempotent, so the one-shot seed Job, `make seed`, and the
// Compose seed service may all run it safely.
//
// The seed only ever creates. When ADMIN_EMAIL already belongs to an account
// (in any tenant) that is not an active owner of this tenant, or the
// address is banned in it, that state is an operator decision: the seed logs
// a warning and skips. It never promotes, adds memberships to, or restores an
// existing account, and never fails the run over it.
//
// The optional tenant ID selects the tenant of the admin membership. Callers
// pass the configured default tenant; when omitted, the seeded default tenant
// (id 1, slug "default") is used.
func Admin(ctx context.Context, cfg config.AdminConfig, stores identityservice.Stores, tx identityservice.Transactor, tenantIDs ...uuid.UUID) error {
	if cfg.Email == "" || cfg.Password == "" {
		slog.Debug("ADMIN_EMAIL or ADMIN_PASSWORD not set, skipping admin seed")
		return nil
	}

	tenantID := tenant.DefaultID
	if len(tenantIDs) > 0 && tenantIDs[0] != uuid.Nil {
		tenantID = tenantIDs[0]
	}
	// The seeder is a trusted system caller: it acts for the platform with no
	// request subject, so mark it for any authz.Authorize on this path.
	ctx = authz.SystemContext(tenant.WithID(ctx, tenantID))

	email := identityservice.NormalizeEmail(cfg.Email)
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "Admin"
	}

	existing, err := stores.Users.GetByEmail(ctx, email)
	if err == nil {
		return keepExisting(ctx, stores, existing, tenantID)
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return fmt.Errorf("look up admin user: %w", err)
	}
	banned, err := stores.EmailBanned(ctx, email)
	if err != nil {
		return fmt.Errorf("check banned admin email: %w", err)
	}
	if banned {
		slog.Warn("ADMIN_EMAIL is banned in the tenant; skipping admin seed",
			slog.String("email", email), slog.String("tenant_id", tenantID.String()))
		return nil
	}

	hash, err := identityservice.HashPassword(cfg.Password)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}
	err = tx.InTx(ctx, func(ctx context.Context, tx identityservice.TxRepos) error {
		user, err := tx.Users.Create(ctx, identityservice.NewUser{Email: email, Name: name, EmailVerified: true})
		if err != nil {
			return err
		}
		if err := tx.Users.CreatePassword(ctx, user.ID, hash); err != nil {
			return err
		}
		if _, err = tx.Memberships.Create(ctx, user.ID, identityservice.RoleOwner); err != nil {
			return err
		}
		return tx.Roles.AssignPlatformRole(ctx, user.ID, authz.RolePlatformAdmin)
	})
	if err != nil {
		// Another concurrent seeder may have won the race; its row decides.
		if errors.Is(err, identityservice.ErrEmailTaken) {
			if winner, lookupErr := stores.Users.GetByEmail(ctx, email); lookupErr == nil {
				return keepExisting(ctx, stores, winner, tenantID)
			}
		}
		return fmt.Errorf("create admin user: %w", err)
	}

	slog.Info("admin user created", slog.String("email", email), slog.String("tenant_id", tenantID.String()))
	return nil
}

// keepExisting leaves an existing account untouched unless it is already an
// active admin member whose email is unverified (a pre-hardening seed), which
// is repaired.
func keepExisting(ctx context.Context, stores identityservice.Stores, user *identityservice.User, tenantID uuid.UUID) error {
	member, err := stores.Memberships.Get(ctx, user.ID)
	switch {
	case errors.Is(err, apperror.ErrNotFound):
		slog.Warn("ADMIN_EMAIL belongs to an account that is not a member of the tenant; skipping admin seed (memberships are never added by the seed)",
			slog.String("email", user.Email), slog.String("tenant_id", tenantID.String()))
		return nil
	case err != nil:
		return fmt.Errorf("look up admin membership: %w", err)
	}
	if !member.Active() || member.Membership.Role != identityservice.RoleOwner {
		slog.Warn("ADMIN_EMAIL belongs to a member that is not an active owner; skipping admin seed (accounts are never promoted or restored)",
			slog.String("email", user.Email),
			slog.String("tenant_id", tenantID.String()),
			slog.String("membership_status", string(member.Membership.Status)),
		)
		return nil
	}
	if !user.EmailVerified() {
		if _, err := stores.Users.VerifyEmail(ctx, user.ID, user.Email); err != nil {
			return fmt.Errorf("mark seeded admin email verified: %w", err)
		}
	}
	slog.Debug("admin user already exists, skipping seed", slog.String("email", user.Email))
	return nil
}
