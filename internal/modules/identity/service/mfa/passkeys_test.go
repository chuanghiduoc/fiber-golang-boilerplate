package mfa

import (
	"context"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
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
	db       *fakes.MemDB
	svc      PasskeyService
	sessions *authn.SessionService
	clock    *mfaClock
	claims   []authn.AccessClaims
	cache    cache.Cache
}

func newPasskeyHarness(t *testing.T) *passkeyHarness {
	t.Helper()
	db := fakes.New()
	h := &passkeyHarness{db: db, clock: &mfaClock{at: time.Unix(1_700_000_000, 0)}}
	h.cache = cache.NewMemoryCache()
	t.Cleanup(func() { _ = h.cache.Close() })
	sign := func(c authn.AccessClaims) (string, error) {
		h.claims = append(h.claims, c)
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			TokenVersion: c.TokenVersion, AMR: c.AMR,
		}, "passkey-test-jwt-secret-000000000000", time.Hour)
	}
	h.sessions = authn.NewSessionService(db.Stores(), fakes.Tx{DB: db}, sign, time.Hour, 720*time.Hour, nil)
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
		Stores: db.Stores(), Tx: fakes.Tx{DB: db}, Cache: h.cache, Sealer: reversibleSealer{},
		Sessions: h.sessions, WebAuthn: relyingParty, Now: h.clock.now,
	})
	h.seedUser(t, fakes.TestID(200), "passkey@example.com")
	return h
}

func (h *passkeyHarness) issueSession(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	pair, err := h.sessions.Issue(fakes.TenantCtx(), userID, 0, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue session: %v", err)
	}
	return pair.SessionID
}

func (h *passkeyHarness) seedUser(t *testing.T, id uuid.UUID, email string) {
	t.Helper()
	now := h.clock.at
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	h.db.Users[id] = &core.User{
		ID: id, Email: email, Name: "Passkey User", Status: core.UserStatusActive,
		EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	h.db.Memberships[[2]uuid.UUID{fakes.TestID(1), id}] = &core.Membership{
		ID: h.db.ID(), TenantID: fakes.TestID(1), UserID: id, Role: core.RoleMember,
		Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
}

func (h *passkeyHarness) register(t *testing.T, authenticator *softauthenticator.Authenticator) *core.Passkey {
	t.Helper()
	ctx := fakes.TenantCtx()
	creation, err := h.svc.BeginRegistration(ctx, fakes.TestID(200), ReauthProof{SessionID: h.issueSession(t, fakes.TestID(200)), CurrentPassword: ""})
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	passkey, err := h.svc.FinishRegistration(ctx, fakes.TestID(200), "Test key",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	if err != nil {
		t.Fatalf("FinishRegistration: %v", err)
	}
	return passkey
}

func (h *passkeyHarness) login(t *testing.T, authenticator *softauthenticator.Authenticator, userID uuid.UUID) *PasskeyLogin {
	t.Helper()
	ctx := fakes.TenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	result, err := h.svc.FinishLogin(ctx,
		authenticator.Assertion(t, assertion.Response.Challenge.String(), PasskeyUserID(userID)), nil)
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
	if passkey.Method() != core.PasskeyMethodHardware {
		t.Fatalf("method = %q, want hwk", passkey.Method())
	}

	result := h.login(t, authenticator, fakes.TestID(200))
	if result.Method != core.PasskeyMethodHardware {
		t.Fatalf("login method = %q, want hwk", result.Method)
	}
	if result.Tokens.AccessToken == "" || result.Tokens.RefreshToken == "" {
		t.Fatal("login must return a token pair")
	}
	if result.Member.User.ID != fakes.TestID(200) {
		t.Fatalf("member = %s", result.Member.User.ID)
	}

	h.db.Mu.Lock()
	session := h.db.Sessions[result.Tokens.SessionID]
	ip := 0
	for _, method := range session.AMR {
		if method == core.PasskeyMethodHardware {
			ip++
		}
	}
	h.db.Mu.Unlock()
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
	result := h.login(t, authenticator, fakes.TestID(200))
	if result.Method != core.PasskeyMethodSoftware {
		t.Fatalf("login method = %q, want swk", result.Method)
	}
}

func TestPasskeySignCountRegressionIsRejected(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)
	h.login(t, authenticator, fakes.TestID(200))

	ctx := fakes.TenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	// The stored counter is 1; presenting 1 again is a replay/clone signal.
	raw := authenticator.AssertionWithCounter(t, assertion.Response.Challenge.String(), PasskeyUserID(fakes.TestID(200)), 1)
	_, err = h.svc.FinishLogin(ctx, raw, nil)
	if code := errorCode(err); code != string(identitycatalog.CodeAuthPasskeySignCount) {
		t.Fatalf("FinishLogin error code = %q, want %q (err=%v)", code, identitycatalog.CodeAuthPasskeySignCount, err)
	}
}

func TestPasskeyCeremonyIsSingleUse(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	ctx := fakes.TenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	challenge := assertion.Response.Challenge.String()
	raw := authenticator.Assertion(t, challenge, PasskeyUserID(fakes.TestID(200)))
	if _, err := h.svc.FinishLogin(ctx, raw, nil); err != nil {
		t.Fatalf("first FinishLogin: %v", err)
	}
	replay := authenticator.Assertion(t, challenge, PasskeyUserID(fakes.TestID(200)))
	if _, err := h.svc.FinishLogin(ctx, replay, nil); codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) == false {
		t.Fatalf("replay error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyCeremonyIsTenantBound(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	assertion, err := h.svc.BeginLogin(fakes.TenantCtx(), "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	otherTenant := tenant.WithID(context.Background(), fakes.TestID(2))
	raw := authenticator.Assertion(t, assertion.Response.Challenge.String(), PasskeyUserID(fakes.TestID(200)))
	if _, err := h.svc.FinishLogin(otherTenant, raw, nil); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("cross-tenant error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyCeremonyExpires(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	assertion, err := h.svc.BeginLogin(fakes.TenantCtx(), "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	h.clock.at = h.clock.at.Add(PasskeyCeremonyTTL + time.Second)
	raw := authenticator.Assertion(t, assertion.Response.Challenge.String(), PasskeyUserID(fakes.TestID(200)))
	if _, err := h.svc.FinishLogin(fakes.TenantCtx(), raw, nil); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("expired ceremony error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyDeleteIsOwnerScoped(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	passkey := h.register(t, authenticator)

	other := fakes.TestID(201)
	h.seedUser(t, other, "other-passkey@example.com")
	if err := h.svc.Delete(fakes.TenantCtx(), other, passkey.ID, ReauthProof{SessionID: h.issueSession(t, other), CurrentPassword: ""}); !codeIs(err, identitycatalog.CodeAuthPasskeyNotFound) {
		t.Fatalf("foreign delete error = %v, want %s", err, identitycatalog.CodeAuthPasskeyNotFound)
	}
	if err := h.svc.Delete(fakes.TenantCtx(), fakes.TestID(200), passkey.ID, ReauthProof{SessionID: h.issueSession(t, fakes.TestID(200)), CurrentPassword: ""}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.svc.List(fakes.TenantCtx(), fakes.TestID(200)); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestPasskeyStepUpAddsMethodAndFreshness(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, authenticator)

	pair, err := h.sessions.Issue(fakes.TenantCtx(), fakes.TestID(200), 0, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ctx := fakes.TenantCtx()
	assertion, err := h.svc.BeginLogin(ctx, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	raw := authenticator.Assertion(t, assertion.Response.Challenge.String(), PasskeyUserID(fakes.TestID(200)))
	verifiedAt, err := h.svc.StepUpPasskey(ctx, fakes.TestID(200), pair.SessionID, raw)
	if err != nil {
		t.Fatalf("StepUpPasskey: %v", err)
	}
	if verifiedAt.IsZero() {
		t.Fatal("step-up must return the verification time")
	}
	h.db.Mu.Lock()
	session := h.db.Sessions[pair.SessionID]
	h.db.Mu.Unlock()
	if session.MFAVerifiedAt == nil {
		t.Fatal("step-up must set mfa_verified_at")
	}
	if !hasAMR(session.AMR, core.PasskeyMethodHardware) {
		t.Fatalf("session amr = %v, want hwk", session.AMR)
	}
}

func TestPasskeyLoginRejectsUnknownCredential(t *testing.T) {
	h := newPasskeyHarness(t)
	registered := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	h.register(t, registered)

	stranger := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	assertion, err := h.svc.BeginLogin(fakes.TenantCtx(), "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	raw := stranger.Assertion(t, assertion.Response.Challenge.String(), PasskeyUserID(fakes.TestID(200)))
	if _, err := h.svc.FinishLogin(fakes.TenantCtx(), raw, nil); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("unknown credential error = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
}

func TestPasskeyBeginRegistrationExcludesExistingCredential(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	passkey := h.register(t, authenticator)

	creation, err := h.svc.BeginRegistration(fakes.TenantCtx(), fakes.TestID(200), ReauthProof{SessionID: h.issueSession(t, fakes.TestID(200)), CurrentPassword: ""})
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
func TestPasskeyChangesRequireReauthentication(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	passkey := h.register(t, authenticator)
	ctx := fakes.TenantCtx()

	if _, err := h.svc.BeginRegistration(ctx, fakes.TestID(200), ReauthProof{}); errorCode(err) != string(identitycatalog.CodeAuthMFAReauthRequired) {
		t.Fatalf("registration without re-authentication err = %v, want mfa_reauth_required", err)
	}
	if err := h.svc.Delete(ctx, fakes.TestID(200), passkey.ID, ReauthProof{}); errorCode(err) != string(identitycatalog.CodeAuthMFAReauthRequired) {
		t.Fatalf("deletion without re-authentication err = %v, want mfa_reauth_required", err)
	}

	passkeys, err := h.svc.List(ctx, fakes.TestID(200))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(passkeys) != 1 {
		t.Fatalf("passkey count = %d, want the rejected deletion to keep it", len(passkeys))
	}
}
func TestPasskeyPasswordAccountRegistrationRequiresCurrentPassword(t *testing.T) {
	h := newPasskeyHarness(t)
	hash, err := core.HashPassword(fakes.Passphrase)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h.db.Mu.Lock()
	h.db.Passwords[fakes.TestID(200)] = hash
	h.db.Mu.Unlock()

	ctx := fakes.TenantCtx()
	if _, err := h.svc.BeginRegistration(ctx, fakes.TestID(200), ReauthProof{SessionID: h.issueSession(t, fakes.TestID(200))}); statusOf(err) != 403 {
		t.Fatalf("registration without a current password err = %v, want 403", err)
	}

	creation, err := h.svc.BeginRegistration(ctx, fakes.TestID(200), ReauthProof{
		SessionID:       h.issueSession(t, fakes.TestID(200)),
		CurrentPassword: fakes.Passphrase,
	})
	if err != nil {
		t.Fatalf("BeginRegistration with the current password: %v", err)
	}
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	passkey, err := h.svc.FinishRegistration(ctx, fakes.TestID(200), "Phone",
		authenticator.Attestation(t, creation.Response.Challenge.String()))
	if err != nil {
		t.Fatalf("FinishRegistration after re-authentication: %v", err)
	}
	if passkey == nil || passkey.Name != "Phone" {
		t.Fatalf("registered passkey = %+v, want Phone", passkey)
	}
}

func TestPasskeyFinishRegistrationRevalidatesSession(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	ctx := fakes.TenantCtx()
	sessionID := h.issueSession(t, fakes.TestID(200))

	creation, err := h.svc.BeginRegistration(ctx, fakes.TestID(200), ReauthProof{SessionID: sessionID})
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	if err := h.sessions.Revoke(ctx, fakes.TestID(200), sessionID); err != nil {
		t.Fatalf("Revoke session: %v", err)
	}
	if _, err := h.svc.FinishRegistration(ctx, fakes.TestID(200), "Revoked",
		authenticator.Attestation(t, creation.Response.Challenge.String())); !codeIs(err, identitycatalog.CodeAuthMFAReauthRequired) {
		t.Fatalf("FinishRegistration after session revocation = %v, want %s", err, identitycatalog.CodeAuthMFAReauthRequired)
	}
	passkeys, err := h.svc.List(ctx, fakes.TestID(200))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(passkeys) != 0 {
		t.Fatalf("passkey count = %d, want no credential after a revoked session", len(passkeys))
	}
}

func TestPasskeyFinishRegistrationRevalidatesMembership(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	ctx := fakes.TenantCtx()
	sessionID := h.issueSession(t, fakes.TestID(200))

	creation, err := h.svc.BeginRegistration(ctx, fakes.TestID(200), ReauthProof{SessionID: sessionID})
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	h.db.Mu.Lock()
	member := h.db.Memberships[[2]uuid.UUID{fakes.TestID(1), fakes.TestID(200)}]
	member.Status = core.MembershipStatusRemoved
	h.db.Mu.Unlock()

	if _, err := h.svc.FinishRegistration(ctx, fakes.TestID(200), "Removed",
		authenticator.Attestation(t, creation.Response.Challenge.String())); statusOf(err) != 401 {
		t.Fatalf("FinishRegistration after membership removal = %v, want 401", err)
	}
	passkeys, err := h.svc.List(ctx, fakes.TestID(200))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(passkeys) != 0 {
		t.Fatalf("passkey count = %d, want no credential after membership removal", len(passkeys))
	}
}

func TestPasskeyFinishRegistrationRequiresCeremony(t *testing.T) {
	h := newPasskeyHarness(t)
	authenticator := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	ctx := fakes.TenantCtx()

	if _, err := h.svc.FinishRegistration(ctx, fakes.TestID(200), "Orphan",
		authenticator.Attestation(t, "no-such-ceremony")); !codeIs(err, identitycatalog.CodeAuthPasskeyInvalid) {
		t.Fatalf("FinishRegistration without a prior begin err = %v, want %s", err, identitycatalog.CodeAuthPasskeyInvalid)
	}
	passkeys, err := h.svc.List(ctx, fakes.TestID(200))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(passkeys) != 0 {
		t.Fatalf("passkey count = %d, want no credential without a ceremony", len(passkeys))
	}
}
