package identity

import (
	"context"
	"io/fs"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	notificationcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/notifications"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit/audithttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module owns authentication, user profiles, sessions, and token lifecycle.
type Module struct {
	auth  *identityhandler.AuthHandler
	users *identityhandler.UserHandler
	// sessions and invitations are set by Build (or WithSessions /
	// WithInvitations); their routes are mounted only when present.
	sessions    *identityhandler.SessionHandler
	invitations *identityhandler.InvitationHandler
	mfa         *identityhandler.MFAHandler
	privacy     *identityhandler.PrivacyHandler
	roles       *identityhandler.RoleHandler
	platform    *identityhandler.PlatformHandler

	// The job, schedule, and startup collaborators are set by Build; a module
	// built with New (tests) has none.
	resetSvc     identityservice.PasswordResetService
	tokenPurger  *identityservice.TokenPurger
	privacySvc   *identityservice.PrivacyService
	tenantPurger *identityservice.TenantPurgeService
	startup      func(ctx context.Context) error
}

var (
	_ app.JobProvider      = (*Module)(nil)
	_ app.ScheduleProvider = (*Module)(nil)
	_ app.PrivacyProvider  = (*Module)(nil)
)

// New builds the module from its handlers. Production uses Build.
func New(auth *identityhandler.AuthHandler, users *identityhandler.UserHandler) *Module {
	return &Module{auth: auth, users: users}
}

func (m *Module) Name() string { return identitycatalog.Owner }

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return identitycatalog.Permissions() }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) ErrorCodes() map[apperror.Code]apperror.Spec { return identitycatalog.ErrorCodes() }

// FeatureFlags implements app.FlagProvider.
func (m *Module) FeatureFlags() []features.Definition { return identitycatalog.FeatureFlags() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return identitycatalog.Locales() }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	rl := deps.Limiters
	// Registration, Google OAuth, and email verification are gated inside the
	// handler because the OAuth callback evaluates its signed-state tenant.
	m.auth.SetFeatureFlags(deps.Flags)
	guard := func(action, resource string) fiber.Handler {
		return audithttp.Guard(deps.Audit, audithttp.Policy{
			Mode:     audithttp.ModeRequired,
			Action:   action,
			Resource: resource,
		})
	}

	auth := v1.Group("/auth")
	// Register is Tier 1: UserService writes auth.registered in the account
	// creation transaction, so no route-level intent row is added.
	auth.Post("/register", rl.Strict, deps.Idempotency, m.auth.Register)
	auth.Post("/login", rl.Strict, guard("auth.login", "auth"), m.auth.Login)
	auth.Post("/refresh", rl.Normal, guard("auth.refresh", "auth"), m.auth.Refresh)
	auth.Post("/logout", rl.Normal, guard("auth.logout", "auth"), m.auth.Logout)
	// logout-all stays JWT-only: it revokes the caller's login sessions
	// (refresh tokens + token version), which an API key does not have.
	auth.Post("/logout-all", middleware.JWTAuthKeys(cfg.JWT.Keyring(), deps.Sessions), rl.Normal, authzhttp.Require(identitycatalog.PermissionSessionsRevoke), guard("auth.logout_all", "auth"), m.auth.LogoutAll)
	auth.Post("/forgot-password", rl.Strict, guard("auth.password_reset_requested", "auth"), m.auth.ForgotPassword)
	auth.Post("/reset-password", rl.Strict, guard("auth.password_reset_completed", "auth"), m.auth.ResetPassword)
	auth.Post("/verify-email", rl.Normal, guard("auth.email_verified", "auth"), m.auth.VerifyEmail)
	auth.Post("/resend-verification", rl.Normal, guard("auth.verification_resent", "auth"), m.auth.ResendVerification)
	auth.Get("/google", rl.Normal, m.auth.GoogleRedirect)
	// The callback is Tier 1: tenant middleware skips it and the tenant comes
	// from the signed state, which a route-level Guard would record too early
	// (tenant 1, anonymous). UserService writes auth.google_login with the
	// state tenant and the signed-in user in the sign-in transaction.
	auth.Get("/google/callback", rl.Normal, m.auth.GoogleCallback)

	// Sessions belong to JWT sign-ins; an API key has none, so these routes
	// are JWT-only like logout-all.
	jwtOnly := middleware.JWTAuthKeys(cfg.JWT.Keyring(), deps.Sessions)
	if m.sessions != nil {
		auth.Get("/sessions", jwtOnly, rl.Relaxed, authzhttp.Require(identitycatalog.PermissionProfileRead), m.sessions.List)
		auth.Delete("/sessions/:id", jwtOnly, rl.Normal, authzhttp.Require(identitycatalog.PermissionSessionsRevoke), guard("auth.session_revoked", "session"), m.sessions.Revoke)
	}
	if m.invitations != nil {
		m.registerInvitationRoutes(v1, auth, jwtOnly, deps)
	}
	if m.mfa != nil {
		// MFA management is JWT-only: an API key has no session to keep and
		// must not change the owner's second factor. Enroll/confirm/regenerate
		// return one-time secrets, so they never take Idempotency-Key.
		auth.Get("/mfa/status", jwtOnly, rl.Relaxed, authzhttp.Require(identitycatalog.PermissionMFARead), m.mfa.Status)
		auth.Post("/mfa/totp/enroll", jwtOnly, rl.Normal, authzhttp.Require(identitycatalog.PermissionMFAWrite), m.mfa.Enroll)
		auth.Post("/mfa/totp/confirm", jwtOnly, rl.Normal, authzhttp.Require(identitycatalog.PermissionMFAWrite), m.mfa.Confirm)
		auth.Post("/mfa/disable", jwtOnly, rl.Normal, authzhttp.Require(identitycatalog.PermissionMFAWrite), m.mfa.Disable)
		auth.Post("/mfa/recovery-codes/regenerate", jwtOnly, rl.Normal, authzhttp.Require(identitycatalog.PermissionMFAWrite), m.mfa.Regenerate)
		// The two-step login verification is public: the sealed, single-use
		// challenge plus the second factor are the credentials.
		auth.Post("/mfa/verify", rl.Strict, m.mfa.Verify)
	}
	if m.privacy != nil {
		m.registerPrivacyRoutes(v1, deps, jwtOnly, guard)
	}

	users := v1.Group("/users", deps.Authenticate())
	users.Get("/me", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionProfileRead), m.users.GetMe)
	users.Put("/me", rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.UpdateMe)
	users.Put("/me/password", rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.ChangePassword)
	users.Get("/:id", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionProfileRead), m.users.GetByID)
	users.Get("/", rl.Relaxed, authzhttp.Require(authz.PermissionAdminUsersRead), m.users.List)
	users.Put("/:id", rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.Update)
	users.Delete("/:id", rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.Delete)

	if m.roles != nil {
		m.registerRoleRoutes(v1, deps)
	}
	if m.platform != nil {
		m.registerPlatformRoutes(v1, deps)
	}
}

// registerRoleRoutes mounts tenant role management. Every mutation is Tier 1:
// RoleService writes its audit row in the mutation transaction.
func (m *Module) registerRoleRoutes(v1 fiber.Router, deps app.Deps) {
	rl := deps.Limiters
	roles := v1.Group("/admin/roles", deps.Authenticate())
	roles.Get("/", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionAdminRolesRead), m.roles.ListRoles)
	roles.Post("/", rl.Normal, authzhttp.Require(identitycatalog.PermissionAdminRolesWrite), deps.Idempotency, m.roles.CreateRole)
	roles.Patch("/:id", rl.Normal, authzhttp.Require(identitycatalog.PermissionAdminRolesWrite), deps.Idempotency, m.roles.UpdateRole)
	roles.Delete("/:id", rl.Normal, authzhttp.Require(identitycatalog.PermissionAdminRolesWrite), deps.Idempotency, m.roles.DeleteRole)
	// The legacy role-change endpoint maps to replacing the member's roles;
	// this one accepts several roles (including tenant-defined ones).
	v1.Put("/admin/users/:id/roles", deps.Authenticate(), rl.Normal,
		authzhttp.Require(authz.PermissionAdminUsersRole), deps.Idempotency, m.roles.ReplaceMemberRoles)
}

// registerPlatformRoutes mounts cross-tenant tenant administration. It is
// protected by platform permissions, which only a platform role assignment
// grants.
func (m *Module) registerPlatformRoutes(v1 fiber.Router, deps app.Deps) {
	rl := deps.Limiters
	// Platform-scope access requires a second factor.
	mfa := authzhttp.RequireMFA()
	tenants := v1.Group("/platform/tenants", deps.Authenticate())
	tenants.Get("/", rl.Relaxed, mfa, authzhttp.Require(authz.PermissionPlatformTenantsRead), m.platform.ListTenants)
	tenants.Post("/", rl.Normal, mfa, authzhttp.Require(authz.PermissionPlatformTenantsWrite), deps.Idempotency, m.platform.CreateTenant)
	tenants.Get("/:id", rl.Relaxed, mfa, authzhttp.Require(authz.PermissionPlatformTenantsRead), m.platform.GetTenant)
	tenants.Patch("/:id/status", rl.Normal, mfa, authzhttp.Require(authz.PermissionPlatformTenantsWrite), deps.Idempotency, m.platform.UpdateTenantStatus)

	if m.mfa != nil {
		// MFA recovery is JWT-only (an API key has no second factor to
		// recover) and runs the same platform chain: limiter, second factor,
		// platform permission, idempotency, handler.
		users := v1.Group("/platform/users", middleware.JWTAuthKeys(deps.Config.JWT.Keyring(), deps.Sessions))
		users.Post("/:id/mfa/reset", rl.Normal, mfa, authzhttp.Require(authz.PermissionPlatformUsersMFAReset), deps.Idempotency, m.mfa.PlatformReset)
	}
}

// registerPrivacyRoutes mounts the caller's exports/erasure and the admin
// erasure variant. Erasure is Tier 1 (the service records in the mutation
// transaction); export creation is Tier 2.
func (m *Module) registerPrivacyRoutes(v1 fiber.Router, deps app.Deps, jwtOnly fiber.Handler, guard func(string, string) fiber.Handler) {
	rl := deps.Limiters
	me := v1.Group("/me", deps.Authenticate())
	me.Post("/data-exports", rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileRead), deps.Idempotency, guard("data_export.requested", "data_export"), m.privacy.CreateExport)
	me.Get("/data-exports", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionProfileRead), m.privacy.ListExports)
	me.Get("/data-exports/:id", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionProfileRead), m.privacy.GetExport)
	me.Get("/data-exports/:id/download", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionProfileRead), m.privacy.DownloadExport)
	me.Post("/erasure", rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.privacy.EraseMe)

	// /admin is shared with the admin module, so middleware stays per route
	// (a group middleware would run on the other module's admin routes).
	// The route is JWT-only: a tenant-scoped API key must not trigger a
	// destructive member erasure (it removes that tenant's membership).
	admin := v1.Group("/admin")
	admin.Post("/users/:id/erasure", jwtOnly, rl.Normal, authzhttp.Require(authz.PermissionAdminUsersDelete), deps.Idempotency, m.privacy.EraseUser)
}

// RegisterPrivacy implements app.PrivacyProvider: identity's own exporter and
// eraser participate in the fan-out.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacySvc == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: m.privacySvc, Eraser: m.privacySvc})
}

// RegisterTenantPurge contributes identity's tenant-wide purger to the tenancy
// registry. It runs last in the purge so the other modules can still reach the
// memberships their rows reference.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry) {
	if m.tenantPurger == nil {
		return
	}
	registry.Register(tenancycontracts.Provider{Name: "identity", Purger: m.tenantPurger})
}

// SetNotifier wires the export-ready notification after notifications.Build.
func (m *Module) SetNotifier(notifier notificationcontracts.Notifier) {
	if m.privacySvc != nil {
		m.privacySvc.SetNotifier(notifier)
	}
}

// registerInvitationRoutes mounts the invitation flow. Create, revoke,
// accept, and sign-up are Tier 1: InvitationService writes invitation.* in
// the mutation transaction, so no route-level Guard is added.
func (m *Module) registerInvitationRoutes(v1, auth fiber.Router, jwtOnly fiber.Handler, deps app.Deps) {
	rl := deps.Limiters
	invitations := v1.Group("/invitations", deps.Authenticate())
	invitations.Post("/", rl.Normal, authzhttp.Require(identitycatalog.PermissionInvitationsWrite), deps.Idempotency, m.invitations.Create)
	invitations.Get("/", rl.Relaxed, authzhttp.Require(identitycatalog.PermissionInvitationsRead), m.invitations.List)
	invitations.Delete("/:id", rl.Normal, authzhttp.Require(identitycatalog.PermissionInvitationsWrite), m.invitations.Revoke)

	// Accepting joins another tenant with the global identity, which a
	// tenant-scoped API key must not do: JWT only. Mounted under /auth (no
	// group middleware) so /invitations' API-key chain does not apply.
	auth.Post("/invitations/accept", jwtOnly, rl.Normal, authzhttp.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.invitations.Accept)
	// Public: the emailed token authorizes creating the invited account.
	auth.Post("/invitations/signup", rl.Strict, deps.Idempotency, m.invitations.SignUp)
}
