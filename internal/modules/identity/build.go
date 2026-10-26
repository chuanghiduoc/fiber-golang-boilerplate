package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepository "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// Provides lists what identity supplies to the platform and to other modules.
// Every cross-module value is a stdlib-only contract.
type Provides struct {
	// Sessions validates JWT sessions/tenants for every module's auth.
	Sessions session.Store
	// TenantResolver resolves X-Tenant-Slug for the /api groups (cached).
	TenantResolver tenant.Resolver
	Principals     identitycontracts.PrincipalReader
	Recipients     identitycontracts.NotificationRecipientLookup
	Admin          identitycontracts.IdentityAdmin
	// TenantActive reports whether a tenant exists and serves requests
	// (trialing or active); the webhooks inbound receiver uses it to reject
	// suspended tenants.
	TenantActive func(ctx context.Context, tenantID int64) (bool, error)
}

// Settings are the configuration sections identity owns. The composition
// root passes them explicitly; shared settings come from infra.Platform.
type Settings struct {
	OAuth config.OAuthConfig
	// Admin is the optional seed admin created at startup.
	Admin config.AdminConfig
}

// Build constructs identity's repositories, services, and handlers from infra.
// It performs no I/O; call Module.Startup for the tenant/admin-seed checks.
func Build(infra app.Infra, settings Settings, privacyRegistry *privacycontracts.Registry, tenancyRegistry *tenancycontracts.Registry) (*Module, Provides, error) {
	cfg := infra.Platform

	var googleOAuth *oauth.GoogleOAuth
	if settings.OAuth.GoogleClientID != "" {
		googleOAuth = oauth.NewGoogleOAuth(settings.OAuth)
		if err := googleOAuth.ValidateFrontendURL(); err != nil {
			return nil, Provides{}, fmt.Errorf("invalid OAuth frontend URL: %w", err)
		}
		slog.Info("Google OAuth enabled")
	}

	stores := identityrepository.NewStores(infra.DB)
	// cmd/api always supplies the DATA_ENCRYPTION_KEY codec. Builds without one
	// (unit/arch tests, an app built with New) fall back to the JWT secret the
	// same way local job payload encryption does.
	payloadCodec := infra.PayloadCodec
	if payloadCodec == nil {
		var err error
		payloadCodec, err = jobs.NewPayloadCodec(cfg.JWT.Secret)
		if err != nil {
			return nil, Provides{}, fmt.Errorf("MFA secret encryption: %w", err)
		}
	}
	sealer := identityadapter.NewPayloadSealer(payloadCodec)
	var mail identityrepository.TxEmailSender
	if infra.Email != nil {
		mail = infra.Email
	}
	// The transactor binds the event bus so user.registered commits with the
	// account (events.Bus.PublishTx); webhooks subscribe durably to it.
	transactor := identityrepository.NewTransactorWithQueue(infra.Tx, infra.Audit, mail, infra.Events, infra.Queue)

	// Every /api/v1 request resolves its tenant before rate limiting, so the
	// slug lookup is cached in-process. A suspension reaches header-resolved
	// requests within tenant.DefaultCacheTTL; authenticated requests also
	// re-check tenant status through the session/API-key validators.
	tenantResolver := tenant.NewCachedResolver(
		identityadapter.NewTenantResolver(stores.Tenants),
		tenant.DefaultCacheTTL, tenant.DefaultNegativeCacheTTL, tenant.DefaultCacheMaxEntries,
	)
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), infra.Cache)
	sessionSvc := identityservice.NewSessionService(stores, transactor, accessTokenSigner(cfg.JWT),
		time.Duration(cfg.JWT.RefreshExpireDays)*24*time.Hour, cfg.JWT.SessionMaxLifetime, sessionStore)
	mfaSvc := identityservice.NewMFAService(identityservice.MFADeps{
		Stores: stores, Tx: transactor, Cache: infra.Cache, Sealer: sealer, Sessions: sessionSvc,
	})

	emailVerifSvc := identityservice.NewEmailVerificationService(identityservice.VerificationDeps{
		Stores: stores, Tx: transactor, Sender: infra.Email, Cache: infra.Cache, FrontendURL: cfg.App.FrontendURL,
	})
	userSvc := identityservice.NewUserService(identityservice.UserDeps{
		Stores:                   stores,
		Tx:                       transactor,
		Cache:                    infra.Cache,
		SessionCache:             sessionStore,
		Sessions:                 sessionSvc,
		Verification:             emailVerifSvc,
		MailTransactional:        transactor.MailTransactional(),
		RequireEmailVerification: cfg.App.RequireEmailVerification,
	})
	passwordResetSvc := identityservice.NewPasswordResetService(identityservice.ResetDeps{
		Stores: stores, Tx: transactor, Sender: infra.Email, Cache: infra.Cache,
		FrontendURL: cfg.App.FrontendURL, SessionCache: sessionStore,
	})
	// Forgot-password requests run as durable jobs so the public endpoint has
	// the same cost and failure modes for registered and unknown addresses.
	// The address in the payload is sealed like email jobs (DATA_ENCRYPTION_KEY).
	if infra.Queue != nil {
		if infra.PayloadCodec == nil {
			return nil, Provides{}, errors.New("password reset job payload encryption: infra.PayloadCodec is required with a job queue")
		}
		identityservice.AttachResetRequestQueue(passwordResetSvc, infra.Queue, infra.PayloadCodec)
	}
	tenantSvc := identityservice.NewTenantService(stores, transactor)
	invitationSvc := identityservice.NewInvitationService(stores, transactor, infra.Email, cfg.App.FrontendURL)
	roleSvc := identityservice.NewRoleService(stores, transactor, sessionStore)

	authHandler := identityhandler.NewAuthHandler(identityhandler.AuthDeps{
		Users:                    userSvc,
		Sessions:                 sessionSvc,
		Resets:                   passwordResetSvc,
		Verification:             emailVerifSvc,
		OAuthStateSecret:         cfg.JWT.Secret,
		RequireEmailVerification: cfg.App.RequireEmailVerification,
		GoogleOAuth:              googleOAuthProvider(googleOAuth),
		Tenants:                  tenantSvc,
		MFA:                      mfaSvc,
	})
	userHandler := identityhandler.NewUserHandler(userSvc, emailVerifSvc)

	identityAdminSvc := identityservice.NewAdminUserService(stores, transactor, sessionStore)

	m := New(authHandler, userHandler)
	m.sessions = identityhandler.NewSessionHandler(sessionSvc)
	m.invitations = identityhandler.NewInvitationHandler(invitationSvc)
	m.mfa = identityhandler.NewMFAHandler(mfaSvc)
	m.roles = identityhandler.NewRoleHandler(roleSvc)
	m.platform = identityhandler.NewPlatformHandler(tenantSvc)
	m.resetSvc = passwordResetSvc
	m.tokenPurger = identityservice.NewTokenPurger(identityrepository.NewTokenPurgeRepository(infra.DB))
	m.privacySvc = identityservice.NewPrivacyService(identityservice.PrivacyDeps{
		Stores:       stores,
		Tx:           transactor,
		Storage:      infra.Storage,
		Registry:     privacyRegistry,
		Verifier:     userSvc,
		SessionCache: sessionStore,
		ExportStore:  identityrepository.NewPrivacyExportRepository(infra.DB),
		AuditEvents:  audit.NewUserEventReader(infra.DB),
	})
	m.privacy = identityhandler.NewPrivacyHandler(m.privacySvc)
	m.tenantPurger = identityservice.NewTenantPurgeService(identityservice.TenantPurgeDeps{
		Repo:     identityrepository.NewTenantPurgeRepository(infra.DB, infra.Tx, infra.Audit),
		Privacy:  m.privacySvc,
		Tenants:  tenantSvc,
		Registry: tenancyRegistry,
		Storage:  infra.Storage,
	})
	m.startup = func(ctx context.Context) error {
		return startup(ctx, infra.Platform, settings.Admin, stores, transactor, sessionStore)
	}

	return m, Provides{
		Sessions:       sessionStore,
		TenantResolver: tenantResolver,
		Principals:     identityadapter.NewPrincipalReader(stores.Memberships),
		Recipients:     identityadapter.NewNotificationRecipient(stores.Memberships),
		Admin:          identityadapter.NewAdminIdentity(identityAdminSvc),
		TenantActive: func(ctx context.Context, id int64) (bool, error) {
			if _, err := stores.Tenants.GetOperationalByID(ctx, id); err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		},
	}, nil
}

// googleOAuthProvider keeps a nil *oauth.GoogleOAuth a nil interface, so the
// handler's "not configured" check works.
func googleOAuthProvider(g *oauth.GoogleOAuth) identityhandler.GoogleOAuthProvider {
	if g == nil {
		return nil
	}
	return g
}

// accessTokenSigner binds session access tokens to the active JWT secret.
func accessTokenSigner(jwt config.JWTConfig) identityservice.AccessTokenSigner {
	return func(c identityservice.AccessClaims) (string, error) {
		return token.GenerateForSession(token.SessionClaims{
			UserID:       c.UserID,
			TenantID:     c.TenantID,
			SessionID:    c.SessionID,
			Email:        c.Email,
			Role:         c.Role,
			TokenVersion: c.TokenVersion,
			AMR:          c.AMR,
		}, jwt.Secret, jwt.ExpireHour)
	}
}

// RegisterJobs implements app.JobProvider: the worker performs queued
// forgot-password requests and the expired-token purge. A module built with
// New (tests) registers nothing.
func (m *Module) RegisterJobs(registrar jobs.Registrar) {
	if m.resetSvc != nil {
		registrar.Register(identityservice.ResetRequestJobKind, m.resetSvc.HandleResetRequestJob)
	}
	if m.tokenPurger != nil {
		registrar.Register(identityservice.TokenPurgeJobKind, m.tokenPurger.HandleJob)
	}
	if m.privacySvc != nil {
		registrar.Register(identityservice.JobKindDataExport, m.privacySvc.HandleExportJob)
		registrar.Register(identityservice.JobKindErase, m.privacySvc.HandleEraseJob)
		registrar.Register(identityservice.JobKindEraseFinalize, m.privacySvc.HandleFinalizeJob)
		registrar.Register(identityservice.JobKindExportPurge, m.privacySvc.HandlePurgeJob)
	}
	if m.tenantPurger != nil {
		registrar.Register(identityservice.TenantPurgeJobKind, m.tenantPurger.HandlePurgeJob)
		registrar.Register(identityservice.TrialExpiryJobKind, m.tenantPurger.HandleTrialExpiryJob)
	}
}

// tokenPurgeSchedule enqueues the expired-token purge. The scheduler is
// optional (SCHEDULER_ENABLED, default false); without it expired tokens are
// still rejected on use but are never deleted.
var tokenPurgeSchedule = scheduler.Spec{
	Name:     "identity.tokens.purge_expired",
	Interval: identityservice.TokenPurgeInterval,
	Kind:     identityservice.TokenPurgeJobKind,
}

// exportPurgeSchedule removes expired data exports and their blobs hourly.
var exportPurgeSchedule = scheduler.Spec{
	Name:     identityservice.JobKindExportPurge,
	Interval: identityservice.ExportPurgeInterval,
	Kind:     identityservice.JobKindExportPurge,
}

// tenantPurgeSchedule erases tenants whose deletion completed.
var tenantPurgeSchedule = scheduler.Spec{
	Name:     identityservice.TenantPurgeJobKind,
	Interval: identityservice.TenantPurgeInterval,
	Kind:     identityservice.TenantPurgeJobKind,
}

// trialExpirySchedule suspends trialing tenants whose trial_ends_at passed.
var trialExpirySchedule = scheduler.Spec{
	Name:     identityservice.TrialExpiryJobKind,
	Interval: identityservice.TrialExpiryInterval,
	Kind:     identityservice.TrialExpiryJobKind,
}

// RegisterSchedules implements app.ScheduleProvider. The spec is a constant,
// so a registration error is a programming error and fails startup loudly.
func (m *Module) RegisterSchedules(registry *scheduler.Registry) {
	if m.tokenPurger != nil {
		if err := registry.Register(tokenPurgeSchedule); err != nil {
			panic(fmt.Sprintf("identity: register %s schedule: %v", tokenPurgeSchedule.Name, err))
		}
	}
	if m.privacySvc != nil {
		if err := registry.Register(exportPurgeSchedule); err != nil {
			panic(fmt.Sprintf("identity: register %s schedule: %v", exportPurgeSchedule.Name, err))
		}
	}
	if m.tenantPurger != nil {
		for _, spec := range []scheduler.Spec{tenantPurgeSchedule, trialExpirySchedule} {
			if err := registry.Register(spec); err != nil {
				panic(fmt.Sprintf("identity: register %s schedule: %v", spec.Name, err))
			}
		}
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

func startup(ctx context.Context, cfg app.PlatformConfig, admin config.AdminConfig,
	stores identityservice.Stores, tx identityservice.Transactor, sessions session.Store,
) error {
	// The baseline seeds tenant id 1. Startup checks existence, not request
	// eligibility: a suspended configured/default tenant must not crash the
	// process. Its requests still return 404 until it is reactivated.
	_, defaultTenant, err := identityservice.StartupTenants(ctx, stores.Tenants, cfg.Tenant.DefaultSlug)
	if err != nil {
		return fmt.Errorf("tenant startup check failed: %w", err)
	}
	if !defaultTenant.Status.Operational() {
		slog.Warn("configured default tenant is not operational; requests for it will return 404 until reactivated",
			slog.String("tenant", defaultTenant.Slug),
			slog.Int64("tenant_id", defaultTenant.ID),
			slog.String("status", string(defaultTenant.Status)),
		)
	}
	if cfg.App.Env == "staging" && !cfg.Tenant.RequireHeader {
		slog.Warn("TENANT_REQUIRE_HEADER=false in staging: header-less requests use the default tenant")
	}

	// The permission catalog is code-owned; write it and the system role
	// grants before the seed assigns roles.
	if err := identityservice.SyncPermissions(ctx, tx, stores.Roles); err != nil {
		return err
	}
	// A deploy can change role_permissions; drop snapshots cached by replicas.
	if err := sessions.InvalidatePermissions(ctx); err != nil {
		return fmt.Errorf("invalidate cached permissions: %w", err)
	}

	// Auto-seed admin user (idempotent) in the configured default tenant.
	if err := seed.Admin(ctx, admin, stores, tx, defaultTenant.ID); err != nil {
		return fmt.Errorf("failed to seed admin user: %w", err)
	}
	return nil
}
