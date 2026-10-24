package identity

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module owns authentication, user profiles, sessions, and token lifecycle.
type Module struct {
	auth  *identityhandler.AuthHandler
	users *identityhandler.UserHandler
}

func New(auth *identityhandler.AuthHandler, users *identityhandler.UserHandler) *Module {
	return &Module{auth: auth, users: users}
}

func (m *Module) Name() string { return "identity" }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	rl := deps.Limiters
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
	auth.Post("/logout-all", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions), rl.Normal, guard("auth.logout_all", "auth"), m.auth.LogoutAll)
	auth.Post("/forgot-password", rl.Strict, guard("auth.password_reset_requested", "auth"), m.auth.ForgotPassword)
	auth.Post("/reset-password", rl.Strict, guard("auth.password_reset_completed", "auth"), m.auth.ResetPassword)
	auth.Post("/verify-email", rl.Normal, guard("auth.email_verified", "auth"), m.auth.VerifyEmail)
	auth.Post("/resend-verification", rl.Normal, guard("auth.verification_resent", "auth"), m.auth.ResendVerification)
	auth.Get("/google", rl.Normal, m.auth.GoogleRedirect)
	auth.Get("/google/callback", rl.Normal, guard("auth.google_login", "auth"), m.auth.GoogleCallback)

	users := v1.Group("/users", apikey.Authenticate(
		middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions),
		deps.APIKeyVerifier,
	))
	users.Get("/me", rl.Relaxed, authz.Require(authz.PermissionProfileRead), m.users.GetMe)
	users.Put("/me", rl.Normal, authz.Require(authz.PermissionProfileWrite), deps.Idempotency, m.users.UpdateMe)
	users.Put("/me/password", rl.Normal, authz.Require(authz.PermissionProfileWrite), deps.Idempotency, m.users.ChangePassword)
	users.Get("/:id", rl.Relaxed, authz.Require(authz.PermissionProfileRead), m.users.GetByID)
	users.Get("/", rl.Relaxed, authz.Require(authz.PermissionAdminUsersRead), m.users.List)
	users.Put("/:id", rl.Normal, authz.Require(authz.PermissionProfileWrite), deps.Idempotency, m.users.Update)
	users.Delete("/:id", rl.Normal, authz.Require(authz.PermissionProfileWrite), deps.Idempotency, m.users.Delete)
}
