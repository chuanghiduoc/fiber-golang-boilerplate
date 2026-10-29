package identity

import (
	"time"

	"context"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/sessiontest"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

type operationSpec struct {
	method string
	path   string
	id     string
}

// expectedOperations is the identity slice of docs/api-routes.md. The
// API-key/notification routes owned by other modules are deliberately absent.
var expectedOperations = []operationSpec{
	{http.MethodPost, "/api/v1/auth/register", "register"},
	{http.MethodPost, "/api/v1/auth/login", "login"},
	{http.MethodPost, "/api/v1/auth/refresh", "refresh"},
	{http.MethodPost, "/api/v1/auth/logout", "logout"},
	{http.MethodPost, "/api/v1/auth/password-resets", "requestPasswordReset"},
	{http.MethodPost, "/api/v1/auth/password-resets/complete", "completePasswordReset"},
	{http.MethodPost, "/api/v1/auth/email-verifications", "requestEmailVerification"},
	{http.MethodPost, "/api/v1/auth/email-verifications/complete", "completeEmailVerification"},
	{http.MethodGet, "/api/v1/auth/google", "startGoogleLogin"},
	{http.MethodGet, "/api/v1/auth/google/callback", "completeGoogleLogin"},
	{http.MethodPost, "/api/v1/auth/google/exchanges", "exchangeGoogleCode"},
	{http.MethodPost, "/api/v1/auth/mfa/verifications", "verifyMFAChallenge"},
	{http.MethodPost, "/api/v1/auth/invitations/accept", "acceptInvitation"},
	{http.MethodPost, "/api/v1/auth/invitations/signup", "signupFromInvitation"},
	{http.MethodPost, "/api/v1/auth/passkeys/options", "startPasskeyLogin"},
	{http.MethodPost, "/api/v1/auth/passkeys/verifications", "completePasskeyLogin"},

	{http.MethodGet, "/api/v1/me/profile", "getMyProfile"},
	{http.MethodPatch, "/api/v1/me/profile", "updateMyProfile"},
	{http.MethodPost, "/api/v1/me/profile/password", "changeMyPassword"},
	{http.MethodGet, "/api/v1/me/sessions", "listMySessions"},
	{http.MethodDelete, "/api/v1/me/sessions/{sessionId}", "deleteMySession"},
	{http.MethodDelete, "/api/v1/me/sessions", "deleteMySessions"},
	{http.MethodGet, "/api/v1/me/mfa", "getMyMFA"},
	{http.MethodPost, "/api/v1/me/mfa/totp", "startTOTPEnrollment"},
	{http.MethodGet, "/api/v1/me/mfa/totp/{factorId}", "getTOTPEnrollment"},
	{http.MethodPost, "/api/v1/me/mfa/totp/{factorId}/confirm", "confirmTOTPEnrollment"},
	{http.MethodPost, "/api/v1/me/mfa/{factorId}/disable", "disableMFAFactor"},
	{http.MethodPost, "/api/v1/me/mfa/recovery-codes", "createRecoveryCodes"},
	{http.MethodPost, "/api/v1/me/mfa/step-up", "stepUpMFA"},
	{http.MethodPost, "/api/v1/me/mfa/passkeys/options", "startPasskeyRegistration"},
	{http.MethodPost, "/api/v1/me/mfa/passkeys", "createPasskey"},
	{http.MethodGet, "/api/v1/me/mfa/passkeys", "listPasskeys"},
	{http.MethodGet, "/api/v1/me/mfa/passkeys/{passkeyId}", "getPasskey"},
	{http.MethodDelete, "/api/v1/me/mfa/passkeys/{passkeyId}", "deletePasskey"},
	{http.MethodGet, "/api/v1/me/data-exports", "listMyDataExports"},
	{http.MethodPost, "/api/v1/me/data-exports", "createDataExport"},
	{http.MethodGet, "/api/v1/me/data-exports/{exportId}", "getDataExport"},
	{http.MethodGet, "/api/v1/me/data-exports/{exportId}/content", "getDataExportContent"},
	{http.MethodPost, "/api/v1/me/erasure-requests", "createMyErasureRequest"},
	{http.MethodGet, "/api/v1/me/erasure-requests", "listMyErasureRequests"},
	{http.MethodGet, "/api/v1/me/erasure-requests/{requestId}", "getMyErasureRequest"},

	{http.MethodGet, "/api/v1/members", "listMembers"},
	{http.MethodGet, "/api/v1/members/{memberId}", "getMember"},
	{http.MethodPatch, "/api/v1/members/{memberId}", "updateMember"},
	{http.MethodDelete, "/api/v1/members/{memberId}", "deleteMember"},
	{http.MethodPost, "/api/v1/members/{memberId}/erasure-requests", "createMemberErasureRequest"},
	{http.MethodGet, "/api/v1/members/{memberId}/erasure-requests/{requestId}", "getMemberErasureRequest"},
	{http.MethodGet, "/api/v1/stats", "getTenantStats"},

	{http.MethodGet, "/api/v1/invitations", "listInvitations"},
	{http.MethodPost, "/api/v1/invitations", "createInvitation"},
	{http.MethodGet, "/api/v1/invitations/{invitationId}", "getInvitation"},
	{http.MethodDelete, "/api/v1/invitations/{invitationId}", "deleteInvitation"},

	{http.MethodGet, "/api/v1/roles", "listRoles"},
	{http.MethodPost, "/api/v1/roles", "createRole"},
	{http.MethodGet, "/api/v1/roles/{roleId}", "getRole"},
	{http.MethodPatch, "/api/v1/roles/{roleId}", "updateRole"},
	{http.MethodDelete, "/api/v1/roles/{roleId}", "deleteRole"},

	{http.MethodGet, "/api/v1/platform/tenants", "listTenants"},
	{http.MethodPost, "/api/v1/platform/tenants", "createTenant"},
	{http.MethodGet, "/api/v1/platform/tenants/{tenantId}", "getTenant"},
	{http.MethodPatch, "/api/v1/platform/tenants/{tenantId}", "updateTenant"},
	{http.MethodDelete, "/api/v1/platform/users/{userId}/mfa", "resetUserMFA"},
}

func routeTestConfig() *config.Config {
	return &config.Config{
		App:       config.AppConfig{Env: "test", BodyLimit: 1 << 20, RequestTimeout: 12 * time.Second},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		JWT:       config.JWTConfig{Secret: "identity-route-test-secret-0123456789", AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
}

// testModule wires every handler object without services. Handler bodies are
// never called here; the test registers and inspects the operation table.
func testModule() *Module {
	m := New(identityhandler.NewAuthHandler(identityhandler.AuthDeps{}), identityhandler.NewUserHandler(nil))
	m.sessions = identityhandler.NewSessionHandler(nil)
	m.invitations = identityhandler.NewInvitationHandler(nil)
	m.members = identityhandler.NewMemberHandler(nil, nil, nil)
	m.mfa = identityhandler.NewMFAHandler(nil)
	m.passkeys = identityhandler.NewPasskeyHandler(nil, nil, nil)
	m.privacy = identityhandler.NewPrivacyHandler(nil)
	m.roles = identityhandler.NewRoleHandler(nil)
	m.platform = identityhandler.NewPlatformHandler(nil)
	return m
}

func registerTestModule(t *testing.T) *app.App {
	t.Helper()
	cfg := routeTestConfig()
	application := app.New(cfg, app.Deps{Audit: audit.Noop{}, Sessions: sessiontest.AllowAll()})
	application.Register(testModule())
	return application
}

func TestRegisterRoutesMatchesRouteMap(t *testing.T) {
	application := registerTestModule(t)

	got := map[string]httpapi.OperationInfo{}
	for _, operation := range application.HTTP().Operations() {
		if operation.Op.Path == "/api/v1" || len(operation.Op.Path) < len("/api/v1/") ||
			operation.Op.Path[:len("/api/v1/")] != "/api/v1/" {
			continue
		}
		got[operation.Op.OperationID] = operation
	}
	require.Len(t, got, len(expectedOperations))
	for _, expected := range expectedOperations {
		operation, ok := got[expected.id]
		require.Truef(t, ok, "operation %q is not registered", expected.id)
		require.Equalf(t, expected.method, operation.Op.Method, "operation %q method", expected.id)
		require.Equalf(t, expected.path, operation.Op.Path, "operation %q path", expected.id)
	}
}

func TestRegisterRoutesPolicies(t *testing.T) {
	application := registerTestModule(t)
	byID := map[string]httpapi.OperationInfo{}
	for _, operation := range application.HTTP().Operations() {
		byID[operation.Op.OperationID] = operation
	}

	cases := []struct {
		id        string
		public    bool
		jwtOnly   bool
		idem      bool
		noStore   bool
		tier1     string
		guard     string
		permitted bool
	}{
		{id: "register", public: true, idem: true, tier1: "auth.registered"},
		{id: "login", public: true, noStore: true, guard: "auth.login"},
		{id: "startGoogleLogin", public: true, noStore: true},
		{id: "completeGoogleLogin", public: true, noStore: true, tier1: "auth.google_login"},
		{id: "deleteMySessions", jwtOnly: true, guard: "auth.logout_all", permitted: true},
		{id: "updateMyProfile", idem: true, tier1: "user.profile_updated", permitted: true},
		{id: "createDataExport", idem: true, guard: "data_export.requested", permitted: true},
		{id: "createMyErasureRequest", jwtOnly: true, idem: true, tier1: "user.erasure_requested", permitted: true},
		{id: "updateMember", idem: true, tier1: "admin.user.updated", permitted: true},
		{id: "updateRole", idem: true, tier1: "admin.role.updated", permitted: true},
		{id: "updateTenant", idem: true, tier1: "tenant.status_changed", permitted: true},
		{id: "resetUserMFA", jwtOnly: true, idem: true, tier1: "admin.user.mfa_reset", permitted: true},
	}
	for _, tc := range cases {
		operation := byID[tc.id]
		require.NotNilf(t, operation.Op.Method, "operation %q missing", tc.id)
		require.Equalf(t, tc.public, operation.Policy.Public, "%s public", tc.id)
		require.Equalf(t, tc.jwtOnly, operation.Policy.JWTOnly, "%s jwtOnly", tc.id)
		require.Equalf(t, tc.idem, operation.Policy.Idempotent, "%s idempotent", tc.id)
		require.Equalf(t, tc.noStore, operation.Policy.NoStore, "%s noStore", tc.id)
		require.Equalf(t, tc.tier1, operation.Policy.Audit.Tier1Action, "%s tier1", tc.id)
		if tc.guard == "" {
			require.Nilf(t, operation.Policy.Audit.Guard, "%s guard", tc.id)
		} else {
			require.NotNilf(t, operation.Policy.Audit.Guard, "%s guard", tc.id)
			require.Equalf(t, tc.guard, operation.Policy.Audit.Guard.Action, "%s guard action", tc.id)
		}
		if tc.permitted {
			require.Truef(t, operation.Policy.Permission != "" || len(operation.Policy.AnyOf) > 0,
				"%s has no permission", tc.id)
		}
	}

	tenants := byID["listTenants"]
	require.True(t, tenants.Policy.RequireMFA, "platform routes require MFA")
	require.True(t, tenants.Policy.RequirePasskey, "platform routes require a passkey")
	require.Equal(t, "platform:tenants:read", string(tenants.Policy.Permission))

	memberUpdate := byID["updateMember"]
	require.ElementsMatch(t, []authz.Permission{
		authz.PermissionAdminUsersRole,
		authz.PermissionAdminUsersBan,
		authz.PermissionAdminUsersDelete,
	}, memberUpdate.Policy.AnyOf)
}

func TestModuleImplementsRouteProvider(t *testing.T) {
	var provider app.RouteProvider = testModule()
	require.Equal(t, "identity", provider.Name())
	_ = context.Background()
}
