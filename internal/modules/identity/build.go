package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepository "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
)

// Provides lists what identity supplies to the platform and to other modules.
// Every cross-module value is a stdlib-only contract.
type Provides struct {
	// Sessions validates JWT token versions/tenants for every module's auth.
	Sessions session.Store
	// TenantResolver resolves X-Tenant-Slug for the /api groups (cached).
	TenantResolver tenant.Resolver
	Principals     identitycontracts.PrincipalReader
	Recipients     identitycontracts.NotificationRecipientLookup
	Admin          identitycontracts.IdentityAdmin
	// TenantActive reports whether a tenant exists and is active; the
	// webhooks inbound receiver uses it to reject suspended tenants.
	TenantActive func(ctx context.Context, tenantID int64) (bool, error)
}

// Build constructs identity's repositories, services, and handlers from infra.
// It performs no I/O; call Module.Startup for the tenant/admin-seed checks.
func Build(infra app.Infra) (*Module, Provides, error) {
	cfg := infra.Config

	var googleOAuth *oauth.GoogleOAuth
	if cfg.OAuth.GoogleClientID != "" {
		googleOAuth = oauth.NewGoogleOAuth(cfg.OAuth)
		if err := googleOAuth.ValidateFrontendURL(); err != nil {
			return nil, Provides{}, fmt.Errorf("invalid OAuth frontend URL: %w", err)
		}
		slog.Info("Google OAuth enabled")
	}

	userRepo := identityrepository.NewUserRepository(infra.Pool)
	tenantRepo := identityrepository.NewTenantRepository(infra.Pool)
	// Every /api/v1 request resolves its tenant before rate limiting, so the
	// slug lookup is cached in-process. A suspension reaches header-resolved
	// requests within tenant.DefaultCacheTTL; authenticated requests also
	// re-check tenant status through the session/API-key validators.
	tenantResolver := tenant.NewCachedResolver(
		identityadapter.NewTenantResolver(tenantRepo),
		tenant.DefaultCacheTTL, tenant.DefaultNegativeCacheTTL, tenant.DefaultCacheMaxEntries,
	)
	sessionStore := session.NewStore(userRepo, infra.Cache)

	refreshTokenRepo := identityrepository.NewRefreshTokenRepository(infra.Pool)
	userSvc := identityservice.NewUserService(userRepo, refreshTokenRepo, cfg.App.RequireEmailVerification,
		infra.Cache, infra.Tx, infra.Audit, sessionStore)
	refreshSvc := identityservice.NewRefreshTokenService(refreshTokenRepo, cfg.JWT.RefreshExpireDays)

	passwordResetRepo := identityrepository.NewPasswordResetRepository(infra.Pool)
	passwordResetSvc := identityservice.NewPasswordResetService(
		userRepo, passwordResetRepo, refreshTokenRepo,
		infra.Email, infra.Cache, cfg.App.FrontendURL, infra.Tx, sessionStore,
	)

	emailVerifRepo := identityrepository.NewEmailVerificationRepository(infra.Pool)
	emailVerifSvc := identityservice.NewEmailVerificationService(
		userRepo, emailVerifRepo, infra.Email, infra.Cache, cfg.App.FrontendURL, infra.Tx, tenantRepo,
	)
	// Registration commits the user, verification token, and email job in one
	// transaction. Resend and email-change paths keep using their own flow.
	userSvc.SetVerificationProvisioner(emailVerifSvc)

	authHandler := identityhandler.NewAuthHandler(
		userSvc, refreshSvc, passwordResetSvc, emailVerifSvc,
		cfg.JWT.Secret, cfg.JWT.ExpireHour, cfg.App.RequireEmailVerification, googleOAuth, tenantRepo,
	)
	userHandler := identityhandler.NewUserHandler(userSvc, emailVerifSvc)

	identityAdminSvc := identityservice.NewAdminUserService(userRepo, refreshTokenRepo, infra.Tx, infra.Audit, sessionStore)

	m := New(authHandler, userHandler)
	m.userSvc = userSvc
	m.startup = func(ctx context.Context) error {
		return startup(ctx, infra, userRepo, tenantRepo)
	}

	return m, Provides{
		Sessions:       sessionStore,
		TenantResolver: tenantResolver,
		Principals:     identityadapter.NewPrincipalReader(userRepo),
		Recipients:     identityadapter.NewNotificationRecipient(userRepo),
		Admin:          identityadapter.NewAdminIdentity(identityAdminSvc),
		TenantActive: func(ctx context.Context, id int64) (bool, error) {
			if _, err := tenantRepo.GetByID(ctx, id); err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		},
	}, nil
}

// AttachWebhooks wires the outbound webhook dispatcher into registration.
// The webhooks module is built after identity, so the composition root
// attaches it once both exist.
func (m *Module) AttachWebhooks(dispatcher webhookcontracts.Dispatcher) {
	if m.userSvc != nil {
		identityservice.AttachWebhookDispatcher(m.userSvc, dispatcher)
	}
}

// Startup runs identity's one-time startup checks: the baseline/default
// tenant must exist, and the admin user is seeded idempotently. A module
// built with New (tests) has no startup work.
func (m *Module) Startup(ctx context.Context) error {
	if m.startup == nil {
		return nil
	}
	return m.startup(ctx)
}

func startup(ctx context.Context, infra app.Infra, userRepo identityrepository.UserRepository, tenantRepo identityrepository.TenantRepository) error {
	cfg := infra.Config
	// The baseline seeds tenant id 1. Startup checks existence, not request
	// eligibility: a suspended configured/default tenant must not crash the
	// process. Its requests still return 404 until it is reactivated.
	_, defaultTenant, err := identityrepository.StartupTenants(ctx, tenantRepo, cfg.Tenant.DefaultSlug)
	if err != nil {
		return fmt.Errorf("tenant startup check failed: %w", err)
	}
	if defaultTenant.Status != "active" {
		slog.Warn("configured default tenant is suspended; requests for it will return 404 until reactivated",
			slog.String("tenant", defaultTenant.Slug),
			slog.Int64("tenant_id", defaultTenant.ID),
		)
	}
	if cfg.App.Env == "staging" && !cfg.Tenant.RequireHeader {
		slog.Warn("TENANT_REQUIRE_HEADER=false in staging: header-less requests use the default tenant")
	}

	// Auto-seed admin user (idempotent) in the configured default tenant.
	if err := seed.Admin(ctx, cfg.Admin, userRepo, defaultTenant.ID); err != nil {
		return fmt.Errorf("failed to seed admin user: %w", err)
	}
	return nil
}
