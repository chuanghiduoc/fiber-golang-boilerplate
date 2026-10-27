//go:build integration

package integration_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// identityKitRefreshTTL is the refresh-token lifetime of kit sessions.
const identityKitRefreshTTL = 7 * 24 * time.Hour

// identityKit wires the identity services and handlers the way identity.Build
// does, for harnesses that mount individual identity routes. The optional
// sender doubles as the transactional outbox, exactly like jobs.QueuedSender.
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
func newIdentityKit(fx *pgtest.Fixture, appCache cache.Cache, recorder audit.TxRecorder, secret string, sender email.Sender) *identityKit {
	stores := identityrepo.NewStores(fx.Scoped(), testutil.MustBanHasher())
	var outbox identityrepo.TxEmailSender
	if sender != nil {
		outbox = senderOutbox{sender}
	}
	tx := identityrepo.NewTransactor(fx.Tx(), recorder, outbox, nil, testutil.MustBanHasher())
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), appCache)
	sign := func(c identityservice.AccessClaims) (string, error) {
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			TokenVersion: c.TokenVersion,
		}, secret, 1*time.Hour)
	}
	sessionSvc := identityservice.NewSessionService(stores, tx, sign, identityKitRefreshTTL, 720*time.Hour, sessionStore)

	kit := &identityKit{stores: stores, sessions: sessionStore}
	if sender != nil {
		kit.verification = identityservice.NewEmailVerificationService(identityservice.VerificationDeps{
			Stores: stores, Tx: tx, Cache: appCache, FrontendURL: "https://frontend.example.test",
		})
		kit.resets = identityservice.NewPasswordResetService(identityservice.ResetDeps{
			Stores: stores, Tx: tx, Cache: appCache,
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

// senderOutbox adapts a plain email.Sender to the transactional outbox port:
// the harness sender records the message when the business transaction asks
// for it, matching jobs.QueuedSender's SendTx behavior.
type senderOutbox struct{ sender email.Sender }

func (s senderOutbox) SendTx(ctx context.Context, _ pgx.Tx, msg email.Message) error {
	return s.sender.Send(ctx, msg)
}
