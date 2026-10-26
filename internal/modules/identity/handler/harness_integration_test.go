//go:build integration

package handler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const harnessSecret = "identity-integration-secret-0000"

// recordingSender captures post-commit emails (the harness has no job
// queue, so mail is never transactional).
type recordingSender struct {
	mu   sync.Mutex
	sent []email.Message
}

func (s *recordingSender) Send(_ context.Context, msg email.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *recordingSender) messages() []email.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]email.Message(nil), s.sent...)
}

// identityHarness wires the identity services over a real database the way
// identity.Build does (minus the job queue).
type identityHarness struct {
	pool         *pgxpool.Pool
	stores       identityservice.Stores
	sessionStore session.Store
	sessions     *identityservice.SessionService
	users        identityservice.UserService
	admin        identityservice.AdminUserService
	invitations  *identityservice.InvitationService
	tenants      *identityservice.TenantService
	roles        identityservice.RoleService
	mail         *recordingSender
	auth         *AuthHandler
}

func newIdentityHarness(t *testing.T, google GoogleOAuthProvider) *identityHarness {
	t.Helper()
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	stores := identityrepo.NewStores(testutil.Scoped(pool))
	transactor := identityrepo.NewTransactor(testutil.NewTxManager(pool), audit.NewPostgresRecorder(testutil.Scoped(pool)), nil, nil)
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), appCache)
	sign := func(c identityservice.AccessClaims) (string, error) {
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			Email: c.Email, Role: c.Role, TokenVersion: c.TokenVersion,
		}, harnessSecret, 1)
	}
	sessions := identityservice.NewSessionService(stores, transactor, sign, time.Hour, 720*time.Hour, sessionStore)
	mail := &recordingSender{}
	verification := identityservice.NewEmailVerificationService(identityservice.VerificationDeps{
		Stores: stores, Tx: transactor, Sender: mail, Cache: appCache, FrontendURL: "https://app.example.test",
	})
	users := identityservice.NewUserService(identityservice.UserDeps{
		Stores: stores, Tx: transactor, Cache: appCache, SessionCache: sessionStore,
		Sessions: sessions, Verification: verification,
	})
	resets := identityservice.NewPasswordResetService(identityservice.ResetDeps{
		Stores: stores, Tx: transactor, Sender: mail, Cache: appCache,
		FrontendURL: "https://app.example.test", SessionCache: sessionStore,
	})
	tenants := identityservice.NewTenantService(stores, transactor)
	h := &identityHarness{
		pool:         pool,
		stores:       stores,
		sessionStore: sessionStore,
		sessions:     sessions,
		users:        users,
		admin:        identityservice.NewAdminUserService(stores, transactor, sessionStore),
		invitations:  identityservice.NewInvitationService(stores, transactor, mail, "https://app.example.test"),
		tenants:      tenants,
		roles:        identityservice.NewRoleService(stores, transactor, sessionStore),
		mail:         mail,
	}
	h.auth = NewAuthHandler(AuthDeps{
		Users: users, Sessions: sessions, Resets: resets, Verification: verification,
		OAuthStateSecret: harnessSecret, GoogleOAuth: google, Tenants: tenants,
	})
	return h
}

type fakeGoogleOAuth struct {
	info *oauth.GoogleUserInfo
}

func (f *fakeGoogleOAuth) AuthURL(state string) string {
	return "https://accounts.example.test/o/oauth2/auth?state=" + state
}

func (f *fakeGoogleOAuth) Exchange(context.Context, string) (*oauth.GoogleUserInfo, error) {
	return f.info, nil
}

func (f *fakeGoogleOAuth) BuildCallbackURL(accessToken, refreshToken string) string {
	return "https://frontend.example.test/auth/callback#access_token=" + accessToken +
		"&refresh_token=" + refreshToken
}

func (f *fakeGoogleOAuth) BuildMFACallbackURL(challenge string) string {
	return "https://frontend.example.test/auth/callback#mfa_required=true&mfa_challenge=" + challenge
}
