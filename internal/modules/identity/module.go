package identity

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// Module owns authentication, user profiles, sessions, and token lifecycle.
type Module struct {
	auth    *identityhandler.AuthHandler
	users   *identityhandler.UserHandler
	members *identityhandler.MemberHandler
	// sessions and invitations are set by Build (or WithSessions /
	// WithInvitations); their routes are mounted only when present.
	sessions    *identityhandler.SessionHandler
	invitations *identityhandler.InvitationHandler
	mfa         *identityhandler.MFAHandler
	passkeys    *identityhandler.PasskeyHandler
	privacy     *identityhandler.PrivacyHandler
	roles       *identityhandler.RoleHandler
	platform    *identityhandler.PlatformHandler

	// The job, schedule, and startup collaborators are set by Build; a module
	// built with New (tests) has none.
	resetSvc     authn.PasswordResetService
	tokenPurger  *authn.TokenPurger
	privacySvc   *privacy.PrivacyService
	tenantPurger *tenancy.TenantPurgeService
	startup      func(ctx context.Context) error

	provides Provides
}

var (
	_ app.JobProvider      = (*Module)(nil)
	_ app.ScheduleProvider = (*Module)(nil)
	_ app.PrivacyProvider  = (*Module)(nil)
	_ app.RouteProvider    = (*Module)(nil)
)

// New builds the module from its handlers. Production uses Build.
func New(auth *identityhandler.AuthHandler, users *identityhandler.UserHandler) *Module {
	return &Module{auth: auth, users: users}
}

func (m *Module) Name() string { return identitycatalog.Owner }

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return identitycatalog.Permissions() }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) Errors() []apperror.Spec { return identitycatalog.Errors() }

// FeatureFlags implements app.FlagProvider.
func (m *Module) FeatureFlags() []features.Definition { return identitycatalog.FeatureFlags() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return identitycatalog.Locales() }

const (
	authTag     = "auth"
	meTag       = "me"
	membersTag  = "members"
	inviteTag   = "invitations"
	rolesTag    = "roles"
	statsTag    = "stats"
	platformTag = "platform"
)

// RegisterRoutes implements app.RouteProvider. Every operation is declared
// through httpapi.Register with its exact policy from docs/api-routes.md.
func (m *Module) RegisterRoutes(api *httpapi.API, deps app.Deps) {
	m.auth.SetFeatureFlags(deps.Flags)
	keyring := deps.Config.JWT.Keyring()
	refreshCookie := identityhandler.RefreshCookieConfig{
		Enabled: deps.Config.JWT.RefreshCookie,
		TTL:     deps.Config.JWT.RefreshTTL,
		Origins: deps.Config.CORS.Origins(),
	}
	m.auth.SetRefreshCookie(refreshCookie)
	if m.sessions != nil {
		m.sessions.SetJWTKeyring(keyring)
	}
	if m.mfa != nil {
		m.mfa.SetJWTKeyring(keyring)
		m.mfa.SetRefreshCookie(refreshCookie)
	}
	if m.passkeys != nil {
		m.passkeys.SetRefreshCookie(refreshCookie)
	}

	guard := func(action, resource string) *httpapi.GuardPolicy {
		return &httpapi.GuardPolicy{Action: action, Resource: resource}
	}

	m.registerAuthRoutes(api, guard)
	m.registerPasskeyRoutes(api, deps)
	m.registerMeRoutes(api, guard)
	m.registerMemberRoutes(api)
	m.registerInvitationRoutes(api)
	m.registerRoleRoutes(api)
	m.registerPlatformRoutes(api)
}

func (m *Module) registerAuthRoutes(api *httpapi.API, guard func(string, string) *httpapi.GuardPolicy) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/register", OperationID: "register",
		Tags: []string{authTag}, Summary: "Register an account",
		Description: "Create a global account and an active membership in the request tenant.",
	}, httpapi.Policy{Public: true, Feature: identitycatalog.KeyUserRegistration, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "auth.registered"}}, m.auth.Register)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/login", OperationID: "login",
		Tags: []string{authTag}, Summary: "Sign in",
		Description: "Authenticate the password and return a token pair, or an MFA challenge when a confirmed factor exists.",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, NoStore: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.login", "auth")}}, m.auth.Login)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/refresh", OperationID: "refresh",
		Tags: []string{authTag}, Summary: "Rotate a refresh token",
	}, httpapi.Policy{Public: true, CookieAuth: true, RateLimit: httpapi.RateLimitNormal, NoStore: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.refresh", "auth")}}, m.auth.Refresh)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/logout", OperationID: "logout",
		Tags: []string{authTag}, Summary: "Revoke a session by refresh-token possession",
	}, httpapi.Policy{Public: true, CookieAuth: true, RateLimit: httpapi.RateLimitNormal, NoStore: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.logout", "auth")}}, m.auth.Logout)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/password-resets", OperationID: "requestPasswordReset",
		Tags: []string{authTag}, Summary: "Request a password reset",
		Description: "Always answers 204: whether the address exists is never revealed.",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.password_reset_requested", "auth")}}, m.auth.ForgotPassword)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/password-resets/complete", OperationID: "completePasswordReset",
		Tags: []string{authTag}, Summary: "Complete a password reset",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.password_reset_completed", "auth")}}, m.auth.ResetPassword)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/email-verifications", OperationID: "requestEmailVerification",
		Tags: []string{authTag}, Summary: "Resend an email verification link",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.verification_resent", "auth")}}, m.auth.ResendVerification)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/email-verifications/complete", OperationID: "completeEmailVerification",
		Tags: []string{authTag}, Summary: "Verify an email address",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.email_verified", "auth")}}, m.auth.VerifyEmail)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/auth/google", OperationID: "startGoogleLogin",
		Tags: []string{authTag}, Summary: "Start Google OAuth",
		Description: "Redirects to Google with PKCE and a signed, tenant-bound state.",
		Errors:      []int{http.StatusFound},
	}, httpapi.Policy{Public: true, Feature: identitycatalog.KeyGoogleOAuth, RateLimit: httpapi.RateLimitNormal}, m.auth.GoogleRedirect)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/auth/google/callback", OperationID: "completeGoogleLogin",
		Tags: []string{authTag}, Summary: "Complete Google OAuth",
		Description: "Redirects with a single-use exchange code in the fragment, or an MFA challenge.",
		Errors:      []int{http.StatusFound},
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitNormal,
		Audit: httpapi.AuditPolicy{Tier1Action: "auth.google_login"}}, m.auth.GoogleCallback)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/auth/google/exchanges", OperationID: "exchangeGoogleCode",
		Tags: []string{authTag}, Summary: "Redeem a Google sign-in code",
	}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, NoStore: true,
		Audit: httpapi.AuditPolicy{Guard: guard("auth.google_exchange", "auth")}}, m.auth.GoogleExchange)

	if m.mfa != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/auth/mfa/verifications", OperationID: "verifyMFAChallenge",
			Tags: []string{authTag}, Summary: "Complete a two-step sign-in",
		}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, NoStore: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "auth.mfa_login"}}, m.mfa.Verify)
	}

	if m.invitations != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/auth/invitations/accept", OperationID: "acceptInvitation",
			Tags: []string{authTag}, Summary: "Accept an invitation",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileWrite, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal, Idempotent: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "invitation.accepted"}}, m.invitations.Accept)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/auth/invitations/signup", OperationID: "signupFromInvitation",
			Tags: []string{authTag}, Summary: "Sign up from an invitation",
		}, httpapi.Policy{Public: true, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "invitation.accepted"}}, m.invitations.SignUp)
	}
}

func (m *Module) registerMeRoutes(api *httpapi.API, guard func(string, string) *httpapi.GuardPolicy) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/me/profile", OperationID: "getMyProfile",
		Tags: []string{meTag}, Summary: "Get my profile",
	}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead, RateLimit: httpapi.RateLimitRelaxed}, m.users.GetMe)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/me/profile", OperationID: "updateMyProfile",
		Tags: []string{meTag}, Summary: "Update my profile",
		Description: "RFC 7396 merge patch. Changing the email requires currentPassword.",
	}, httpapi.Policy{Permission: identitycatalog.PermissionProfileWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: httpapi.AuditPolicy{Tier1Action: "user.profile_updated"}}, m.users.UpdateMe)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/me/profile/password", OperationID: "changeMyPassword",
		Tags: []string{meTag}, Summary: "Change my password",
	}, httpapi.Policy{Permission: identitycatalog.PermissionProfileWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, NoStore: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "user.password_changed"}}, m.users.ChangePassword)

	if m.sessions != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/sessions", OperationID: "listMySessions",
			Tags: []string{meTag}, Summary: "List my sessions",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead, JWTOnly: true,
			RateLimit: httpapi.RateLimitRelaxed}, m.sessions.List)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodDelete, Path: "/api/v1/me/sessions/{sessionId}", OperationID: "deleteMySession",
			Tags: []string{meTag}, Summary: "Revoke one of my sessions",
		}, httpapi.Policy{Permission: identitycatalog.PermissionSessionsRevoke, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal,
			Audit:     httpapi.AuditPolicy{Guard: guard("auth.session_revoked", "session")}}, m.sessions.Revoke)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodDelete, Path: "/api/v1/me/sessions", OperationID: "deleteMySessions",
			Tags: []string{meTag}, Summary: "Log out everywhere",
		}, httpapi.Policy{Permission: identitycatalog.PermissionSessionsRevoke, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal,
			Audit:     httpapi.AuditPolicy{Guard: guard("auth.logout_all", "auth")}}, m.auth.LogoutAll)
	}

	if m.mfa != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/mfa", OperationID: "getMyMFA",
			Tags: []string{meTag}, Summary: "Get my MFA status",
		}, httpapi.Policy{Permission: identitycatalog.PermissionMFARead, JWTOnly: true,
			RateLimit: httpapi.RateLimitRelaxed, NoStore: true}, m.mfa.Status)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/me/mfa/totp", OperationID: "startTOTPEnrollment",
			Tags: []string{meTag}, Summary: "Start TOTP enrollment",
		}, httpapi.Policy{Permission: identitycatalog.PermissionMFAWrite, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal, NoStore: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "auth.mfa_enrollment_started"}}, m.mfa.Enroll)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/me/mfa/totp/{factorId}/confirm", OperationID: "confirmTOTPEnrollment",
			Tags: []string{meTag}, Summary: "Confirm TOTP enrollment",
		}, httpapi.Policy{Permission: identitycatalog.PermissionMFAWrite, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal, NoStore: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "auth.mfa_enabled"}}, m.mfa.Confirm)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/me/mfa/{factorId}/disable", OperationID: "disableMFAFactor",
			Tags: []string{meTag}, Summary: "Disable an MFA factor",
		}, httpapi.Policy{Permission: identitycatalog.PermissionMFAWrite, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal,
			Audit:     httpapi.AuditPolicy{Tier1Action: "auth.mfa_disabled"}}, m.mfa.Disable)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/me/mfa/recovery-codes", OperationID: "createRecoveryCodes",
			Tags: []string{meTag}, Summary: "Regenerate recovery codes",
		}, httpapi.Policy{Permission: identitycatalog.PermissionMFAWrite, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal, NoStore: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "auth.mfa_recovery_codes_regenerated"}}, m.mfa.Regenerate)
	}

	if m.privacy != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/data-exports", OperationID: "listMyDataExports",
			Tags: []string{meTag}, Summary: "List my data exports",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead,
			RateLimit: httpapi.RateLimitRelaxed}, m.privacy.ListExports)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/me/data-exports", OperationID: "createDataExport",
			Tags: []string{meTag}, Summary: "Request a data export",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead,
			RateLimit: httpapi.RateLimitNormal, Idempotent: true,
			Audit: httpapi.AuditPolicy{Guard: guard("data_export.requested", "data_export")}}, m.privacy.CreateExport)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/data-exports/{exportId}", OperationID: "getDataExport",
			Tags: []string{meTag}, Summary: "Get a data export",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead,
			RateLimit: httpapi.RateLimitRelaxed}, m.privacy.GetExport)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/data-exports/{exportId}/content", OperationID: "getDataExportContent",
			Tags: []string{meTag}, Summary: "Download a data export",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead,
			RateLimit: httpapi.RateLimitRelaxed, NoStore: true}, m.privacy.DownloadExport)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/me/erasure-requests", OperationID: "createMyErasureRequest",
			Tags: []string{meTag}, Summary: "Request erasure of my account data",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileWrite, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal, Idempotent: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "user.erasure_requested"}}, m.privacy.EraseMe)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/erasure-requests", OperationID: "listMyErasureRequests",
			Tags: []string{meTag}, Summary: "List my erasure requests",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead, JWTOnly: true,
			RateLimit: httpapi.RateLimitRelaxed}, m.privacy.ListErasureRequests)

		httpapi.Register(api, httpapi.Op{
			Method: http.MethodGet, Path: "/api/v1/me/erasure-requests/{requestId}", OperationID: "getMyErasureRequest",
			Tags: []string{meTag}, Summary: "Get my erasure request",
		}, httpapi.Policy{Permission: identitycatalog.PermissionProfileRead, JWTOnly: true,
			RateLimit: httpapi.RateLimitRelaxed}, m.privacy.GetErasureRequest)
	}
}

func (m *Module) registerMemberRoutes(api *httpapi.API) {
	if m.members == nil {
		return
	}
	anyMemberWrite := []authz.Permission{authz.PermissionAdminUsersRole, authz.PermissionAdminUsersBan}

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/members", OperationID: "listMembers",
		Tags: []string{membersTag}, Summary: "List members",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.members.List)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/members/{memberId}", OperationID: "getMember",
		Tags: []string{membersTag}, Summary: "Get a member",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.members.Get)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/members/{memberId}", OperationID: "updateMember",
		Tags: []string{membersTag}, Summary: "Update a member",
		Description: "RFC 7396 merge patch of roleIds and status (active, suspended, or removed).",
	}, httpapi.Policy{AnyOf: anyMemberWrite, RateLimit: httpapi.RateLimitNormal,
		Idempotent: true, Audit: httpapi.AuditPolicy{Tier1Action: "admin.user.updated"}}, m.members.Update)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodDelete, Path: "/api/v1/members/{memberId}", OperationID: "deleteMember",
		Tags: []string{membersTag}, Summary: "Remove a member",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersDelete,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "admin.user.removed"}}, m.members.Delete)

	if m.privacy != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/members/{memberId}/erasure-requests", OperationID: "createMemberErasureRequest",
			Tags: []string{membersTag}, Summary: "Erase a member's tenant data",
		}, httpapi.Policy{Permission: authz.PermissionAdminUsersDelete, JWTOnly: true,
			RateLimit: httpapi.RateLimitNormal, Idempotent: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "admin.user.erasure_requested"}}, m.privacy.EraseUser)
	}

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/stats", OperationID: "getTenantStats",
		Tags: []string{statsTag}, Summary: "Get tenant statistics",
	}, httpapi.Policy{Permission: authz.PermissionAdminStatsRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.members.Stats)
}

func (m *Module) registerInvitationRoutes(api *httpapi.API) {
	if m.invitations == nil {
		return
	}
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/invitations", OperationID: "listInvitations",
		Tags: []string{inviteTag}, Summary: "List pending invitations",
	}, httpapi.Policy{Permission: identitycatalog.PermissionInvitationsRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.invitations.List)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/invitations", OperationID: "createInvitation",
		Tags: []string{inviteTag}, Summary: "Invite a member",
	}, httpapi.Policy{Permission: identitycatalog.PermissionInvitationsWrite,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "invitation.created"}}, m.invitations.Create)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/invitations/{invitationId}", OperationID: "getInvitation",
		Tags: []string{inviteTag}, Summary: "Get an invitation",
	}, httpapi.Policy{Permission: identitycatalog.PermissionInvitationsRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.invitations.Get)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodDelete, Path: "/api/v1/invitations/{invitationId}", OperationID: "deleteInvitation",
		Tags: []string{inviteTag}, Summary: "Revoke an invitation",
	}, httpapi.Policy{Permission: identitycatalog.PermissionInvitationsWrite,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "invitation.revoked"}}, m.invitations.Revoke)
}

func (m *Module) registerRoleRoutes(api *httpapi.API) {
	if m.roles == nil {
		return
	}
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/roles", OperationID: "listRoles",
		Tags: []string{rolesTag}, Summary: "List roles",
	}, httpapi.Policy{Permission: identitycatalog.PermissionAdminRolesRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.roles.ListRoles)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/roles", OperationID: "createRole",
		Tags: []string{rolesTag}, Summary: "Create a tenant role",
	}, httpapi.Policy{Permission: identitycatalog.PermissionAdminRolesWrite,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "admin.role.created"}}, m.roles.CreateRole)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/roles/{roleId}", OperationID: "getRole",
		Tags: []string{rolesTag}, Summary: "Get a role",
	}, httpapi.Policy{Permission: identitycatalog.PermissionAdminRolesRead,
		RateLimit: httpapi.RateLimitRelaxed}, m.roles.GetRole)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/roles/{roleId}", OperationID: "updateRole",
		Tags: []string{rolesTag}, Summary: "Update a tenant role",
	}, httpapi.Policy{Permission: identitycatalog.PermissionAdminRolesWrite,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "admin.role.updated"}}, m.roles.UpdateRole)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodDelete, Path: "/api/v1/roles/{roleId}", OperationID: "deleteRole",
		Tags: []string{rolesTag}, Summary: "Delete a tenant role",
	}, httpapi.Policy{Permission: identitycatalog.PermissionAdminRolesWrite,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "admin.role.deleted"}}, m.roles.DeleteRole)
}

func (m *Module) registerPlatformRoutes(api *httpapi.API) {
	if m.platform == nil {
		return
	}
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/platform/tenants", OperationID: "listTenants",
		Tags: []string{platformTag}, Summary: "List tenants",
	}, httpapi.Policy{Permission: authz.PermissionPlatformTenantsRead, RequireMFA: true,
		RateLimit: httpapi.RateLimitRelaxed}, m.platform.ListTenants)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/platform/tenants", OperationID: "createTenant",
		Tags: []string{platformTag}, Summary: "Create a tenant",
	}, httpapi.Policy{Permission: authz.PermissionPlatformTenantsWrite, RequireMFA: true,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "tenant.created"}}, m.platform.CreateTenant)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/platform/tenants/{tenantId}", OperationID: "getTenant",
		Tags: []string{platformTag}, Summary: "Get a tenant",
	}, httpapi.Policy{Permission: authz.PermissionPlatformTenantsRead, RequireMFA: true,
		RateLimit: httpapi.RateLimitRelaxed}, m.platform.GetTenant)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/platform/tenants/{tenantId}", OperationID: "updateTenant",
		Tags: []string{platformTag}, Summary: "Update a tenant",
	}, httpapi.Policy{Permission: authz.PermissionPlatformTenantsWrite, RequireMFA: true,
		RateLimit: httpapi.RateLimitNormal, Idempotent: true,
		Audit: httpapi.AuditPolicy{Tier1Action: "tenant.status_changed"}}, m.platform.UpdateTenant)

	if m.mfa != nil {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodDelete, Path: "/api/v1/platform/users/{userId}/mfa", OperationID: "resetUserMFA",
			Tags: []string{platformTag}, Summary: "Reset a user's MFA",
		}, httpapi.Policy{Permission: authz.PermissionPlatformUsersMFAReset, RequireMFA: true,
			JWTOnly: true, RateLimit: httpapi.RateLimitNormal, Idempotent: true,
			Audit: httpapi.AuditPolicy{Tier1Action: "admin.user.mfa_reset"}}, m.mfa.PlatformReset)
	}
}

// RegisterPrivacy implements app.PrivacyProvider: identity's own exporter and
// eraser participate in the fan-out.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacySvc == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "identity", Exporter: m.privacySvc, Eraser: m.privacySvc})
}

// TenantPurgers contributes identity's tenant-wide purger to the composition
// root's tenancy registry. It runs last in the purge so the other modules can
// still reach the memberships their rows reference.
func (m *Module) TenantPurgers() []tenancycontracts.Provider {
	if m.tenantPurger == nil {
		return nil
	}
	return []tenancycontracts.Provider{{Name: "identity", Purger: m.tenantPurger}}
}

// SessionStore returns the JWT session validator the app installs.
func (m *Module) SessionStore() session.Store { return m.provides.Sessions }

// TenantResolver returns the cached tenant slug resolver.
func (m *Module) TenantResolver() tenant.Resolver { return m.provides.TenantResolver }

// Principals returns the API-key principal reader.
func (m *Module) Principals() identitycontracts.PrincipalReader { return m.provides.Principals }

// MemberRanks returns the tenant role-hierarchy checker.
func (m *Module) MemberRanks() identitycontracts.MemberRankChecker { return m.provides.MemberRanks }

// Recipients returns the notification recipient lookup.
func (m *Module) Recipients() identitycontracts.NotificationRecipientLookup {
	return m.provides.Recipients
}

// Admin returns the identity half of the admin surface.
func (m *Module) Admin() identitycontracts.IdentityAdmin { return m.provides.Admin }

// TenantActive reports whether a tenant serves requests (used by webhooks).
func (m *Module) TenantActive() func(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	return m.provides.TenantActive
}
