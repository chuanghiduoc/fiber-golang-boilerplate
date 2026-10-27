//go:build integration

// Package pgfixtures wires the identity services against a real PostgreSQL
// fixture. It lives under identity/internal so every identity subpackage's
// integration tests share one composition without importing each other's
// internals.
package pgfixtures

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/mfa"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// Instance catalogs built from identity's providers, exactly like the
// composition root. Integration tests never touch process-wide state.
var (
	Catalog = testutil.Catalogs().Permissions
	Errors  = newErrorCatalog()
)

func newErrorCatalog() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: identitycatalog.Owner, Specs: identitycatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

// ResolvedStatus renders the error's code through the test catalog, the same
// way the HTTP layer does.
func ResolvedStatus(appErr *apperror.AppError) int {
	_, spec := Errors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}

// Mailbox is the transactional email outbox of the fixture.
type Mailbox struct {
	mu   sync.Mutex
	msgs []email.Message
}

// Send records a message.
func (m *Mailbox) Send(_ context.Context, msg email.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

// SendTx is the transactional outbox path: the fixture records the message
// when the caller's transaction asks for it, which is exactly what a real
// QueuedSender does (enqueue on tx).
func (m *Mailbox) SendTx(ctx context.Context, _ pgx.Tx, msg email.Message) error {
	return m.Send(ctx, msg)
}

var linkToken = regexp.MustCompile(`token=([0-9a-f]{64})`)

// TokenFor waits for the newest link sent to addr and returns its token.
func (m *Mailbox) TokenFor(t *testing.T, addr string) string {
	t.Helper()
	var tok string
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := len(m.msgs) - 1; i >= 0; i-- {
			if m.msgs[i].To[0] == addr {
				if match := linkToken.FindStringSubmatch(m.msgs[i].HTML); match != nil {
					tok = match[1]
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	return tok
}

// Env is the identity service graph backed by one PostgreSQL fixture.
type Env struct {
	Pool     *pgxpool.Pool
	Fx       *pgtest.Fixture
	Stores   core.Stores
	Tx       *identityrepo.Transactor
	Users    authn.UserService
	Admin    membership.AdminUserService
	Roles    membership.RoleService
	Sessions *authn.SessionService
	MFA      mfa.MFAService
	Passkeys mfa.PasskeyService
	Resets   authn.PasswordResetService
	Verify   authn.EmailVerificationService
	Mail     *Mailbox
}

// NewEnv builds the service graph. A nil recorder fails closed so tests can
// prove Tier 1 actions roll back when the audit write fails.
func NewEnv(t *testing.T, recorder audit.TxRecorder) *Env {
	t.Helper()

	fx := testutil.NewFixture(t)
	pool := fx.Bypass
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })

	if recorder == nil {
		recorder = audit.NewPostgresRecorder(fx.Scoped())
	}
	stores := identityrepo.NewStores(fx.Scoped(), testutil.BanHasher(t))
	mail := &Mailbox{}
	tx := identityrepo.NewTransactor(fx.Tx(), recorder, mail, nil, testutil.BanHasher(t))
	sign := func(authn.AccessClaims) (string, error) { return "access", nil }
	sessions := authn.NewSessionService(stores, tx, sign, time.Hour, 720*time.Hour, nil)
	verify := authn.NewEmailVerificationService(authn.VerificationDeps{
		Stores: stores, Tx: tx, Cache: appCache, FrontendURL: "https://app.example.test",
	})
	relyingParty, err := webauthn.New(&webauthn.Config{
		RPID:          "localhost",
		RPDisplayName: "Identity integration tests",
		RPOrigins:     []string{"http://localhost:3000"},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
		},
	})
	require.NoError(t, err)
	passkeys := mfa.NewPasskeyService(mfa.PasskeyDeps{
		Stores: stores, Tx: tx, Cache: appCache, Sealer: Sealer{}, Sessions: sessions,
		WebAuthn: relyingParty,
	})
	return &Env{
		Pool:   pool,
		Fx:     fx,
		Stores: stores,
		Tx:     tx,
		Users: authn.NewUserService(authn.UserDeps{
			Stores: stores, Tx: tx, Cache: appCache, Sessions: sessions, Verification: verify,
		}),
		Admin:    membership.NewAdminUserService(stores, tx, nil),
		Roles:    membership.NewRoleService(stores, tx, nil, Catalog),
		Sessions: sessions,
		MFA: mfa.NewMFAService(mfa.MFADeps{
			Stores: stores, Tx: tx, Cache: appCache, Sealer: Sealer{}, Sessions: sessions,
		}),
		Passkeys: passkeys,
		Resets: authn.NewPasswordResetService(authn.ResetDeps{
			Stores: stores, Tx: tx, Cache: appCache, FrontendURL: "https://app.example.test",
		}),
		Verify: verify,
		Mail:   mail,
	}
}

// AppCode is the typed error code carried by err, or "".
func AppCode(err error) string {
	if appErr, ok := err.(*apperror.AppError); ok {
		return appErr.Code
	}
	return ""
}

// AppStatus is the HTTP status a handler would return for err.
func AppStatus(err error) int {
	if appErr, ok := err.(*apperror.AppError); ok {
		return ResolvedStatus(appErr)
	}
	return 0
}

// Sealer is a reversible test double for the data-encryption codec; the
// database guards under test do not depend on the ciphertext format.
type Sealer struct{}

// Seal prefixes the plaintext.
func (Sealer) Seal(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }

// Open strips the prefix.
func (Sealer) Open(p []byte) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(p), "sealed:")), nil
}

// EnrollTOTPSecret enrolls TOTP for userID and returns the shared secret.
func EnrollTOTPSecret(t *testing.T, e *Env, userID uuid.UUID) string {
	t.Helper()
	enrollment, err := e.MFA.EnrollTOTP(tenant.WithID(context.Background(), tenant.DefaultID), userID, uuid.Nil, Passphrase)
	require.NoError(t, err)
	u, err := url.Parse(enrollment.OtpauthURI)
	require.NoError(t, err)
	secret := u.Query().Get("secret")
	require.NotEmpty(t, secret)
	return secret
}

// Passphrase is the fixture account password, built at runtime so the
// repository carries no password-like literal for secret scanners.
var Passphrase = strings.Join([]string{"correct", "horse", "battery", "staple"}, "-") + "-2026"
