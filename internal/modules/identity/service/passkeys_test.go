package service

import (
	"context"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/softauthenticator"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const (
	passkeyTestRPID   = "localhost"
	passkeyTestOrigin = "http://localhost:3000"
)

type passkeyHarness struct {
	db       *memDB
	svc      PasskeyService
	sessions *SessionService
	clock    *mfaClock
	claims   []AccessClaims
	cache    cache.Cache
}

func newPasskeyHarness(t *testing.T) *passkeyHarness {
	t.Helper()
	db := newMemDB()
	h := &passkeyHarness{db: db, clock: &mfaClock{at: time.Unix(1_700_000_000, 0)}}
	h.cache = cache.NewMemoryCache()
	t.Cleanup(func() { _ = h.cache.Close() })
	sign := func(c AccessClaims) (string, error) {
		h.claims = append(h.claims, c)
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			TokenVersion: c.TokenVersion, AMR: c.AMR,
		}, "passkey-test-jwt-secret-000000000000", time.Hour)
	}
	h.sessions = NewSessionService(db.stores(), memTx{db}, sign, time.Hour, 720*time.Hour, nil)
	relyingParty, err := webauthn.New(&webauthn.Config{
		RPID:          passkeyTestRPID,
		RPDisplayName: "Test",
		RPOrigins:     []string{passkeyTestOrigin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
		},
	})
	if err != nil {
		t.Fatalf("webauthn.New: %v", err)
	}
	h.svc = NewPasskeyService(PasskeyDeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: h.cache, Sealer: reversibleSealer{},
		Sessions: h.sessions, WebAuthn: relyingParty, Now: h.clock.now,
	})
	h.seedUser(t, testID(200), "passkey@example.com")
	return h
}

func (h *passkeyHarness) seedUser(t *testing.T, id uuid.UUID, email string) {
	t.Helper()
	now := h.clock.at
	h.db.mu.Lock()
	defer h.db.mu.Unlock()
	h.db.users[id] = &User{
		ID: id, Email: email, Name: "Passkey User", Status: UserStatusActive,
		EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	h.db.memberships[[2]uuid.UUID{testID(1), id}] = &Membership{
		ID: h.db.id(), TenantID: testID(1), UserID: id, Role: RoleMember,
		Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
}

func (h *passkeyHarness) register(t *testing.T, authenticator *softauthenticator.Authenticator) *Passkey {
	t.Helper()
	ctx := testTenantCtx()
	creation, err := h.svc.BeginRegistration(ctx, testID(200))
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	passkey, err := h.svc.FinishRegistration(ctx, testID(200), "Test key",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	if err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}
	return passkey
}

func (h *passkeyHarness) login(t *testing.T, authenticator *softauthenticator.Authenticator, userID uuid.UUID) *PasskeyLogin {
	t.Helper()
	ctx := testTenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	result, err := h.svc.FinishLogin(ctx,
		authenticator.Assertion(t, assertion.Response.Challenge.String(), passkeyUserID(userID)), nil)
	if err != nil {
		t.Fatalf("FinishLogin: %v", err)
	}
	return result
}

func TestPasskeyRegisterAndLogin(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)

	passkey := h.register(t, authenticator)
	if passkey.Name != "Test key" {
		t.Fatalf("name = %q", passkey.Name)
	}
	if passkey.SignCount != 0 {
		t.Fatalf("sign count = %d, want 0", passkey.SignCount)
	}
	if passkey.Method() != PasskeyMethodHardware {
		t.Fatalf("method = %q, want hwk", passkey.Method())
	}

	result := h.login(t, authenticator, testID(200))
	if result.Method != PasskeyMethodHardware {
		t.Fatalf("login method = %q, want hwk", result.Method)
	}
	if result.Tokens.AccessToken == "" || result.Tokens.RefreshToken == "" {
		t.Fatal("login must return a token pair")
	}
	if result.Member.User.ID != testID(200) {
		t.Fatalf("member = %s", result.Member.User.ID)
	}

	h.db.mu.Lock()
	session := h.db.sessions[result.Tokens.SessionID]
	ip := 0
	for _, method := range session.AMR {
		if method == PasskeyMethodHardware {
			ip++
		}
	}
	h.db.mu.Unlock()
	if ip != 1 {
		t.Fatalf("session amr = %v, want exactly one hwk", session.AMR)
	}
	if session.MFAVerifiedAt == nil {
		t.Fatal("passkey login must set sessions.mfa_verified_at")
	}
}

func TestPasskeySyncedCredentialUsesSoftwareKeyAMR(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	authenticator.BackupEligible = true
	authenticator.BackupState = true

	h.register(t, authenticator)
	result := h.login(t, authenticator, testID(200))
	if result.Method != PasskeyMethodSoftware {
		t.Fatalf("login method = %q, want swk", result.Method)
	}
}

func TestPasskeySignCountRegressionIsRejected(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)
	h.login(t, authenticator, testID(200))

	ctx := testTenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	// The stored counter is 1; presenting 1 again is a replay/clone signal.
	raw := authenticator.AssertionWithCounter(t, assertion.Response.Challenge.String(), passkeyUserID(testID(200)), 1)
	_, err = h.svc.FinishLogin(ctx, raw, nil)
	if code := errorCode(err); code != string(identitycatalog.CodeAuthPasskeySignCount) {
		t.Fatalf("FinishLogin error code = %q, want %q (err=%v)", code, identitycatalog.CodeAuthPasskeySignCount, err)
	}
}

func TestPasskeyCeremonyIsSingleUse(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	ctx := testTenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	challenge := assertion.Response.Challenge.String()
	raw := authenticator.Assertion(t, challenge, passkeyUserID(testID(200)))
	if _, err := h.svc.FinishLogin(ctx, raw, nil); err != nil {
		t.Fatalf("first FinishLogin: %v", err)
	}
	replay := authenticator.Assertion(t, challenge, passkeyUserID(testID(200)))
	if _, err := h.svc.FinishLogin(ctx, replay, nil); codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) == false {
		t.Fatalf("replay error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyCeremonyIsTenantBound(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	assertion, err := h.svc.BeginLogin(testTenantCtx(), "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	otherTenant := tenant.WithID(context.Background(), testID(2))
	raw := authenticator.Assertion(t, assertion.Response.Challenge.String(), passkeyUserID(testID(200)))
	if _, err := h.svc.FinishLogin(otherTenant, raw, nil); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("cross-tenant error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyCeremonyExpires(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	assertion, err := h.svc.BeginLogin(testTenantCtx(), "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	h.clock.at = h.clock.at.Add(PasskeyCeremonyTTL + time.Second)
	raw := authenticator.Assertion(t, assertion.Response.Challenge.String(), passkeyUserID(testID(200)))
	if _, err := h.svc.FinishLogin(testTenantCtx(), raw, nil); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("expired ceremony error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyDeleteIsOwnerScoped(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	passkey := h.register(t, authenticator)

	other := testID(201)
	h.seedUser(t, other, "other-passkey@example.com")
	if err := h.svc.Delete(testTenantCtx(), other, passkey.ID); !codeIs(err, identitycatalog.CodeAuthPasskeyNotFound) {
		t.Fatalf("foreign delete error = %v, want %s", err, identitycatalog.CodeAuthPasskeyNotFound)
	}
	if err := h.svc.Delete(testTenantCtx(), testID(200), passkey.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.svc.List(testTenantCtx(), testID(200)); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestPasskeyStepUpAddsMethodAndFreshness(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	pair, err := h.sessions.Issue(testTenantCtx(), testID(200), 0, []string{AMRPassword})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ctx := testTenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	raw := authenticator.Assertion(t, assertion.Response.Challenge.String(), passkeyUserID(testID(200)))
	verifiedAt, err := h.svc.StepUpPasskey(ctx, testID(200), pair.SessionID, raw)
	if err != nil {
		t.Fatalf("StepUpPasskey: %v", err)
	}
	if verifiedAt.IsZero() {
		t.Fatal("step-up must return the verification time")
	}
	h.db.mu.Lock()
	session := h.db.sessions[pair.SessionID]
	h.db.mu.Unlock()
	if session.MFAVerifiedAt == nil {
		t.Fatal("step-up must set mfa_verified_at")
	}
	if !hasAMR(session.AMR, PasskeyMethodHardware) {
		t.Fatalf("session amr = %v, want hwk", session.AMR)
	}
}

func TestPasskeyLoginRejectsUnknownCredential(t *testing.T) {
	h := newPasskeyHarness(t)
	registered := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, registered)

	stranger := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	assertion, err := h.svc.BeginLogin(testTenantCtx(), "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	raw := stranger.Assertion(t, assertion.Response.Challenge.String(), passkeyUserID(testID(200)))
	if _, err := h.svc.FinishLogin(testTenantCtx(), raw, nil); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("unknown credential error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyBeginRegistrationExcludesExistingCredential(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	passkey := h.register(t, authenticator)

	creation, err := h.svc.BeginRegistration(testTenantCtx(), testID(200))
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	if len(creation.Response.CredentialExcludeList) != 1 {
		t.Fatalf("exclusions = %d, want 1", len(creation.Response.CredentialExcludeList))
	}
	if string(creation.Response.CredentialExcludeList[0].CredentialID) != string(passkey.CredentialID) {
		t.Fatal("exclusion does not contain the registered credential id")
	}
}

func codeIs(err error, code apperror.Code) bool {
	return errorCode(err) == string(code)
}

func hasAMR(amr []string, method string) bool {
	for _, value := range amr {
		if value == method {
			return true
		}
	}
	return false
}
