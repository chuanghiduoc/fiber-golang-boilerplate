package identity

import (
	"context"
	"io/fs"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module owns authentication, user profiles, sessions, and token lifecycle.
type Module struct {
	auth  *identityhandler.AuthHandler
	users *identityhandler.UserHandler

	// userSvc and startup are set by Build; a module built with New (tests)
	// has neither.
	userSvc     identityservice.UserService
	resetSvc    identityservice.PasswordResetService
	tokenPurger *identityservice.TokenPurger
	startup     func(ctx context.Context) error
}

var (
	_ app.JobProvider      = (*Module)(nil)
	_ app.ScheduleProvider = (*Module)(nil)
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
		return audit.Guard(deps.Audit, audit.Policy{
			Mode:     audit.ModeRequired,
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
	auth.Post("/logout-all", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions), rl.Normal, authz.Require(identitycatalog.PermissionSessionsRevoke), guard("auth.logout_all", "auth"), m.auth.LogoutAll)
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

	users := v1.Group("/users", deps.Authenticate())
	users.Get("/me", rl.Relaxed, authz.Require(identitycatalog.PermissionProfileRead), m.users.GetMe)
	users.Put("/me", rl.Normal, authz.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.UpdateMe)
	users.Put("/me/password", rl.Normal, authz.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.ChangePassword)
	users.Get("/:id", rl.Relaxed, authz.Require(identitycatalog.PermissionProfileRead), m.users.GetByID)
	users.Get("/", rl.Relaxed, authz.Require(authz.PermissionAdminUsersRead), m.users.List)
	users.Put("/:id", rl.Normal, authz.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.Update)
	users.Delete("/:id", rl.Normal, authz.Require(identitycatalog.PermissionProfileWrite), deps.Idempotency, m.users.Delete)
}
