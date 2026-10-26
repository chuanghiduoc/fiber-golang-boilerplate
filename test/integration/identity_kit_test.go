//go:build integration

package integration_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// identityKitRefreshTTL is the refresh-token lifetime of kit sessions.
const identityKitRefreshTTL = 7 * 24 * time.Hour

// identityKit wires the identity services and handlers the way identity.Build
// does, for harnesses that mount individual identity routes. It has no email
// outbox: mail goes through the optional post-commit sender.
type identityKit struct {
	stores   identityservice.Stores
	sessions session.Store
	users    identityservice.UserService
	admin    identityservice.AdminUserService
	// verification and resets are nil unless the kit was given a sender.
	verification identityservice.EmailVerificationService
	resets       identityservice.PasswordResetService
	auth         *identityhandler.AuthHandler
	userHandler  *identityhandler.UserHandler
}

// newIdentityKit signs access tokens with secret. sender may be nil, which
// leaves email verification and password reset unwired.
func newIdentityKit(pool *pgxpool.Pool, appCache cache.Cache, recorder audit.TxRecorder, secret string, sender email.Sender) *identityKit {
	stores := identityrepo.NewStores(testutil.Scoped(pool))
	tx := identityrepo.NewTransactor(testutil.NewTxManager(pool), recorder, nil, nil)
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), appCache)
	sign := func(c identityservice.AccessClaims) (string, error) {
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			Email: c.Email, Role: c.Role, TokenVersion: c.TokenVersion,
		}, secret, 1)
	}
	sessionSvc := identityservice.NewSessionService(stores, tx, sign, identityKitRefreshTTL, 720*time.Hour, sessionStore)

	kit := &identityKit{stores: stores, sessions: sessionStore}
	if sender != nil {
		kit.verification = identityservice.NewEmailVerificationService(identityservice.VerificationDeps{
			Stores: stores, Tx: tx, Sender: sender, Cache: appCache, FrontendURL: "https://frontend.example.test",
		})
		kit.resets = identityservice.NewPasswordResetService(identityservice.ResetDeps{
			Stores: stores, Tx: tx, Sender: sender, Cache: appCache,
			FrontendURL: "https://frontend.example.test", SessionCache: sessionStore,
		})
	}
	kit.users = identityservice.NewUserService(identityservice.UserDeps{
		Stores:       stores,
		Tx:           tx,
		Cache:        appCache,
		SessionCache: sessionStore,
		Sessions:     sessionSvc,
		Verification: kit.verification,
	})
	kit.admin = identityservice.NewAdminUserService(stores, tx, sessionStore)
	kit.auth = identityhandler.NewAuthHandler(identityhandler.AuthDeps{
		Users:            kit.users,
		Sessions:         sessionSvc,
		Resets:           kit.resets,
		Verification:     kit.verification,
		OAuthStateSecret: secret,
	})
	kit.userHandler = identityhandler.NewUserHandler(kit.users, kit.verification)
	return kit
}
