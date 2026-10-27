package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepository "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityauthn "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	identitycore "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitymembership "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	identitymfa "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/mfa"
	identityprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/privacy"
	identitytenancy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
	"github.com/google/uuid"
)

// Provides lists what identity supplies to the platform and to other modules.
// Every cross-module value is a stdlib-only contract.
type Provides struct {
	// Sessions validates JWT sessions/tenants for every module's auth.
	Sessions session.Store
	// TenantResolver resolves X-Tenant-Slug for the /api groups (cached).
	TenantResolver tenant.Resolver
	Principals     identitycontracts.PrincipalReader
	// MemberRanks applies the tenant role hierarchy for another module that
	// manages a member's resources (API-key ownership).
	MemberRanks identitycontracts.MemberRankChecker
	Recipients  identitycontracts.NotificationRecipientLookup
	Admin       identitycontracts.IdentityAdmin
	// TenantActive reports whether a tenant exists and serves requests
	// (trialing or active); the webhooks inbound receiver uses it to reject
	// suspended tenants.
	TenantActive func(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// Settings are the configuration sections identity owns. The composition
// root passes them explicitly; shared settings come from infra.Platform.
type Settings struct {
	OAuth config.OAuthConfig
	// DataEncryption supplies the pepper for the keyed ban marker. It is the
	// same key set that seals job payloads (DATA_ENCRYPTION_KEY plus its
	// previous keys), domain-separated by platform/banhash.
	DataEncryption config.DataEncryptionConfig
	// WebAuthn configures the passkey relying party. An empty RP ID disables
	// passkey ceremonies (they fail closed with 503).
	WebAuthn config.WebAuthnConfig
}

// Build constructs identity's repositories, services, and handlers from infra.
// It performs no I/O; call Module.Startup for the tenant and permission-catalog
// checks. The admin user is created only by cmd/seed, never at startup.
// Deps are the contracts identity consumes from other modules.
type Deps struct {
	// Privacy receives identity's exporters and erasers.
	Privacy *privacycontracts.Registry
	// Tenancy receives identity's tenant-wide purger.
	Tenancy *tenancycontracts.Registry
}

func Build(infra app.Infra, settings Settings, deps Deps) (*Module, error) {
	cfg := infra.Platform
	if infra.Email == nil {
		return nil, errors.New("identity: the transactional email outbox is required")
	}

	var googleOAuth *oauth.GoogleOAuth
	if settings.OAuth.GoogleClientID != "" {
		googleOAuth = oauth.NewGoogleOAuth(settings.OAuth)
		if err := googleOAuth.ValidateFrontendURL(); err != nil {
			return nil, fmt.Errorf("invalid OAuth frontend URL: %w", err)
		}
		slog.Info("Google OAuth enabled")
	}

	banHasher, err := banhash.New(
		settings.DataEncryption.ResolvedKey(),
		settings.DataEncryption.PreviousKeyList()...,
	)
	if err != nil {
		return nil, fmt.Errorf("ban marker key: %w", err)
	}
	stores := identityrepository.NewStores(infra.DB, banHasher)
	// Security notifications commit their email.send.encrypted job with the
	// change on the transactional outbox.
	securityNotifier := identityauthn.NewSecurityNotifier(stores.Tenants, infra.Email)
	// cmd/api always supplies the DATA_ENCRYPTION_KEY codec. Builds without one
	// (unit/arch tests, an app built with New) derive it from the same
	// dedicated key; there is no JWT_SECRET fallback.
	payloadCodec := infra.PayloadCodec
	if payloadCodec == nil {
		var err error
		payloadCodec, err = jobs.NewPayloadCodec(settings.DataEncryption.ResolvedKey())
		if err != nil {
			return nil, fmt.Errorf("MFA secret encryption: %w", err)
		}
	}
	sealer := identityadapter.NewPayloadSealer(payloadCodec)
	mail := infra.Email
	// The transactor binds the event bus so user.registered commits with the
	// account (events.Bus.PublishTx); webhooks subscribe durably to it.
	transactor := identityrepository.NewTransactorWithQueue(infra.Tx, infra.Audit, mail, infra.Events, infra.Queue, banHasher)

	// Every /api/v1 request resolves its tenant before rate limiting, so the
	// slug lookup is cached in-process. A suspension reaches header-resolved
	// requests within tenant.DefaultCacheTTL; authenticated requests also
	// re-check tenant status through the session/API-key validators.
	tenantResolver := tenant.NewCachedResolver(
		identityadapter.NewTenantResolver(stores.Tenants),
		tenant.DefaultCacheTTL, tenant.DefaultNegativeCacheTTL, tenant.DefaultCacheMaxEntries,
	)
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), infra.Cache)
	sessionSvc := identityauthn.NewSessionService(stores, transactor, accessTokenSigner(cfg.JWT),
		cfg.JWT.RefreshTTL, cfg.JWT.SessionMaxLifetime, sessionStore)
	mfaSvc := identitymfa.NewMFAService(identitymfa.MFADeps{
		Stores: stores, Tx: transactor, Cache: infra.Cache, Sealer: sealer, Sessions: sessionSvc,
		Security: securityNotifier,
	})
	passkeySvc := identitymfa.PasskeyService(nil)
	if settings.WebAuthn.Enabled() {
		relyingParty, err := webauthn.New(&webauthn.Config{
			RPID:          strings.TrimSpace(settings.WebAuthn.RPID),
			RPDisplayName: settings.WebAuthn.DisplayName(),
			RPOrigins:     settings.WebAuthn.OriginList(),
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				UserVerification: protocol.VerificationRequired,
				ResidentKey:      protocol.ResidentKeyRequirementRequired,
			},
			Timeouts: webauthn.TimeoutsConfig{
				Login: webauthn.TimeoutConfig{
					Enforce:    true,
					Timeout:    identitymfa.PasskeyCeremonyTTL,
					TimeoutUVD: identitymfa.PasskeyCeremonyTTL,
				},
				Registration: webauthn.TimeoutConfig{
					Enforce:    true,
					Timeout:    identitymfa.PasskeyCeremonyTTL,
					TimeoutUVD: identitymfa.PasskeyCeremonyTTL,
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("invalid WebAuthn configuration: %w", err)
		}
		passkeySvc = identitymfa.NewPasskeyService(identitymfa.PasskeyDeps{
			Stores: stores, Tx: transactor, Cache: infra.Cache, Sealer: sealer,
			Sessions: sessionSvc, WebAuthn: relyingParty, Security: securityNotifier,
		})
	}

	emailVerifSvc := identityauthn.NewEmailVerificationService(identityauthn.VerificationDeps{
		Stores: stores, Tx: transactor, Cache: infra.Cache, FrontendURL: cfg.App.FrontendURL,
	})
	userSvc := identityauthn.NewUserService(identityauthn.UserDeps{
		Stores:                   stores,
		Tx:                       transactor,
		Cache:                    infra.Cache,
		SessionCache:             sessionStore,
		Sessions:                 sessionSvc,
		Verification:             emailVerifSvc,
		RequireEmailVerification: cfg.App.RequireEmailVerification,
		Security:                 securityNotifier,
	})
	passwordResetSvc := identityauthn.NewPasswordResetService(identityauthn.ResetDeps{
		Stores: stores, Tx: transactor, Cache: infra.Cache,
		FrontendURL: cfg.App.FrontendURL, SessionCache: sessionStore, Security: securityNotifier,
	})
	// Forgot-password requests run as durable jobs so the public endpoint has
	// the same cost and failure modes for registered and unknown addresses.
	// The address in the payload is sealed like email jobs (DATA_ENCRYPTION_KEY).
	if infra.Queue != nil {
		if infra.PayloadCodec == nil {
			return nil, errors.New("password reset job payload encryption: infra.PayloadCodec is required with a job queue")
		}
		identityauthn.AttachResetRequestQueue(passwordResetSvc, infra.Queue, infra.PayloadCodec)
	}
	tenantSvc := identitytenancy.NewTenantService(stores, transactor)
	invitationSvc := identitymembership.NewInvitationService(stores, transactor, cfg.App.FrontendURL)
	roleSvc := identitymembership.NewRoleService(stores, transactor, sessionStore, infra.Catalogs.Permissions)

	authHandler := identityhandler.NewAuthHandler(identityhandler.AuthDeps{
		Users:                    userSvc,
		Sessions:                 sessionSvc,
		Resets:                   passwordResetSvc,
		Verification:             emailVerifSvc,
		OAuthStateSecret:         settings.OAuth.StateKey,
		RequireEmailVerification: cfg.App.RequireEmailVerification,
		GoogleOAuth:              googleOAuthProvider(googleOAuth),
		Tenants:                  tenantSvc,
		Cache:                    infra.Cache,
		Sealer:                   sealer,
		MFA:                      mfaSvc,
	})
	userHandler := identityhandler.NewUserHandler(userSvc, emailVerifSvc)

	identityAdminSvc := identitymembership.NewAdminUserService(stores, transactor, sessionStore)

	m := New(authHandler, userHandler)
	m.sessions = identityhandler.NewSessionHandler(sessionSvc)
	m.invitations = identityhandler.NewInvitationHandler(invitationSvc, roleSvc)
	m.members = identityhandler.NewMemberHandler(identityAdminSvc, userSvc, roleSvc)
	m.mfa = identityhandler.NewMFAHandler(mfaSvc)
	var stepUp identitymfa.MFAStepUpService
	if service, ok := mfaSvc.(identitymfa.MFAStepUpService); ok {
		stepUp = service
	}
	m.passkeys = identityhandler.NewPasskeyHandler(passkeySvc, stepUp, authHandler)
	m.roles = identityhandler.NewRoleHandler(roleSvc)
	m.platform = identityhandler.NewPlatformHandler(tenantSvc)
	m.resetSvc = passwordResetSvc
	m.tokenPurger = identityauthn.NewTokenPurger(identityrepository.NewTokenPurgeRepository(infra.DB))
	m.privacySvc = identityprivacy.NewPrivacyService(identityprivacy.PrivacyDeps{
		Stores:       stores,
		Tx:           transactor,
		Storage:      infra.Storage,
		Registry:     deps.Privacy,
		Events:       infra.Events,
		Verifier:     userSvc,
		SessionCache: sessionStore,
		ExportStore:  identityrepository.NewPrivacyExportRepository(infra.DB),
		AuditEvents:  audit.NewUserEventReader(infra.DB),
	})
	m.privacy = identityhandler.NewPrivacyHandler(m.privacySvc)
	m.tenantPurger = identitytenancy.NewTenantPurgeService(identitytenancy.TenantPurgeDeps{
		Repo:     identityrepository.NewTenantPurgeRepository(infra.DB, infra.Tx, infra.Audit),
		Privacy:  m.privacySvc,
		Tenants:  tenantSvc,
		Registry: deps.Tenancy,
		Storage:  infra.Storage,
	})
	m.startup = func(ctx context.Context) error {
		return startup(ctx, infra.Platform, stores, transactor, sessionStore, infra.Catalogs.Permissions)
	}

	m.provides = Provides{
		Sessions:       sessionStore,
		TenantResolver: tenantResolver,
		Principals:     identityadapter.NewPrincipalReader(stores.Memberships),
		MemberRanks:    identityadapter.NewMemberRank(stores, transactor),
		Recipients:     identityadapter.NewNotificationRecipient(stores.Memberships),
		Admin:          identityadapter.NewAdminIdentity(identityAdminSvc),
		TenantActive: func(ctx context.Context, id uuid.UUID) (bool, error) {
			if _, err := stores.Tenants.GetOperationalByID(ctx, id); err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		},
	}
	return m, nil
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
func accessTokenSigner(jwt config.JWTConfig) identityauthn.AccessTokenSigner {
	return func(c identityauthn.AccessClaims) (string, error) {
		return token.GenerateForSession(token.SessionClaims{
			UserID:       c.UserID,
			TenantID:     c.TenantID,
			SessionID:    c.SessionID,
			TokenVersion: c.TokenVersion,
			AMR:          c.AMR,
		}, jwt.Secret, jwt.AccessTTL)
	}
}

// RegisterJobs implements app.JobProvider: the worker performs queued
// forgot-password requests and the expired-token purge. A module built with
// New (tests) registers nothing.
func (m *Module) RegisterJobs(registrar jobs.Registrar) {
	if m.resetSvc != nil {
		registrar.Register(identityauthn.ResetRequestJobKind, m.resetSvc.HandleResetRequestJob)
	}
	if m.tokenPurger != nil {
		registrar.Register(identityauthn.TokenPurgeJobKind, m.tokenPurger.HandleJob)
	}
	if m.privacySvc != nil {
		registrar.Register(identityprivacy.JobKindDataExport, m.privacySvc.HandleExportJob)
		registrar.Register(identityprivacy.JobKindErase, m.privacySvc.HandleEraseJob)
		registrar.Register(identityprivacy.JobKindEraseFinalize, m.privacySvc.HandleFinalizeJob)
		registrar.Register(identityprivacy.JobKindExportPurge, m.privacySvc.HandlePurgeJob)
	}
	if m.tenantPurger != nil {
		registrar.Register(identitytenancy.TenantPurgeJobKind, m.tenantPurger.HandlePurgeJob)
		registrar.Register(identitytenancy.TrialExpiryJobKind, m.tenantPurger.HandleTrialExpiryJob)
		registrar.Register(identitytenancy.TenantDeletionJobKind, m.tenantPurger.HandleDeletionJob)
	}
}

// tokenPurgeSchedule enqueues the expired-token purge. The scheduler is
// optional (SCHEDULER_ENABLED, default false); without it expired tokens are
// still rejected on use but are never deleted.
var tokenPurgeSchedule = scheduler.Spec{
	Name:     "identity.tokens.purge_expired",
	Interval: identityauthn.TokenPurgeInterval,
	Kind:     identityauthn.TokenPurgeJobKind,
}

// exportPurgeSchedule removes expired data exports and their blobs hourly.
var exportPurgeSchedule = scheduler.Spec{
	Name:     identityprivacy.JobKindExportPurge,
	Interval: identityprivacy.ExportPurgeInterval,
	Kind:     identityprivacy.JobKindExportPurge,
}

// tenantPurgeSchedule erases tenants whose deletion completed.
var tenantPurgeSchedule = scheduler.Spec{
	Name:     identitytenancy.TenantPurgeJobKind,
	Interval: identitytenancy.TenantPurgeInterval,
	Kind:     identitytenancy.TenantPurgeJobKind,
}

// trialExpirySchedule suspends trialing tenants whose trial_ends_at passed.
var trialExpirySchedule = scheduler.Spec{
	Name:     identitytenancy.TrialExpiryJobKind,
	Interval: identitytenancy.TrialExpiryInterval,
	Kind:     identitytenancy.TrialExpiryJobKind,
}

// tenantDeletionSchedule moves pending_deletion tenants past their grace
// period to deleted, so the purge job can pick them up.
var tenantDeletionSchedule = scheduler.Spec{
	Name:     identitytenancy.TenantDeletionJobKind,
	Interval: identitytenancy.TenantDeletionInterval,
	Kind:     identitytenancy.TenantDeletionJobKind,
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
		for _, spec := range []scheduler.Spec{tenantPurgeSchedule, trialExpirySchedule, tenantDeletionSchedule} {
			if err := registry.Register(spec); err != nil {
				panic(fmt.Sprintf("identity: register %s schedule: %v", spec.Name, err))
			}
		}
	}
}

// Startup runs identity's one-time startup checks: the baseline/default
// tenant must exist and the permission catalog is synchronized. It never
// creates or promotes accounts; the admin user comes from cmd/seed. A module
// built with New (tests) has no startup work.
func (m *Module) Startup(ctx context.Context) error {
	if m.startup == nil {
		return nil
	}
	return m.startup(ctx)
}

func startup(ctx context.Context, cfg app.PlatformConfig,
	stores identitycore.Stores, tx identitycore.Transactor, sessions session.Store,
	catalog *authz.Catalog,
) error {
	// The baseline seeds tenant id 1. Startup checks existence, not request
	// eligibility: a suspended configured/default tenant must not crash the
	// process. Its requests still return 404 until it is reactivated.
	_, defaultTenant, err := identitytenancy.StartupTenants(ctx, stores.Tenants, cfg.Tenant.DefaultSlug)
	if err != nil {
		return fmt.Errorf("tenant startup check failed: %w", err)
	}
	if !defaultTenant.Status.Operational() {
		slog.Warn("configured default tenant is not operational; requests for it will return 404 until reactivated",
			slog.String("tenant", defaultTenant.Slug),
			slog.String("tenant_id", defaultTenant.ID.String()),
			slog.String("status", string(defaultTenant.Status)),
		)
	}
	if cfg.App.Env == "staging" && !cfg.Tenant.RequireHeader {
		slog.Warn("TENANT_REQUIRE_HEADER=false in staging: header-less requests use the default tenant")
	}
	if err := warnUnmatchedBanMarkers(ctx, stores); err != nil {
		return err
	}

	// The permission catalog is code-owned; write it and the system role
	// grants here so cmd/seed can assign roles against a known catalog.
	if err := identitymembership.SyncPermissions(ctx, tx, stores.Roles, catalog); err != nil {
		return err
	}
	// A deploy can change role_permissions; drop snapshots cached by replicas.
	if err := sessions.InvalidatePermissions(ctx); err != nil {
		return fmt.Errorf("invalidate cached permissions: %w", err)
	}
	return nil
}

// warnUnmatchedBanMarkers reports retained ban markers whose key id is no
// longer in the configured data-encryption key set. Such a ban can no longer
// match an address (and cannot be re-hashed after erasure), so the address may
// be able to rejoin the tenant. It is a warning, not a startup failure: the
// operator may have removed the key deliberately.
func warnUnmatchedBanMarkers(ctx context.Context, stores identitycore.Stores) error {
	if stores.Bans == nil {
		return nil
	}
	counts, err := stores.Memberships.BannedMarkerCounts(ctx)
	if err != nil {
		return fmt.Errorf("ban marker startup check: %w", err)
	}
	known := make(map[string]struct{})
	for _, id := range stores.Bans.KeyIDs() {
		known[id] = struct{}{}
	}
	for _, count := range counts {
		if _, ok := known[count.KeyID]; ok {
			continue
		}
		keyID := count.KeyID
		if keyID == "" {
			keyID = "unkeyed"
		}
		slog.Warn("ban markers reference a data-encryption key that is not configured; those bans cannot match until it is restored",
			slog.String("banned_email_hash_kid", keyID),
			slog.Int64("bans", count.Bans),
		)
	}
	return nil
}
