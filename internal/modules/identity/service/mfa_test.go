package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
)

// reversibleSealer is a deterministic test sealer; production uses
// jobs.PayloadCodec (AES-256-GCM).
type reversibleSealer struct{}

func (reversibleSealer) Seal(p []byte) ([]byte, error) {
	return append([]byte("sealed:"), p...), nil
}

func (reversibleSealer) Open(p []byte) ([]byte, error) {
	if !strings.HasPrefix(string(p), "sealed:") {
		return nil, errors.New("not sealed")
	}
	return p[len("sealed:"):], nil
}

type mfaClock struct{ at time.Time }

func (c *mfaClock) now() time.Time { return c.at }

type mfaHarness struct {
	db       *memDB
	svc      MFAService
	sessions *SessionService
	clock    *mfaClock
	claims   []AccessClaims
}

func newMFAHarness(t *testing.T) *mfaHarness {
	t.Helper()
	db := newMemDB()
	h := &mfaHarness{db: db, clock: &mfaClock{at: time.Unix(1_700_000_000, 0)}}

	sign := func(c AccessClaims) (string, error) {
		h.claims = append(h.claims, c)
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			Email: c.Email, Role: c.Role, TokenVersion: c.TokenVersion, AMR: c.AMR,
		}, "mfa-test-jwt-secret-0000000000000", 1)
	}
	h.sessions = NewSessionService(db.stores(), memTx{db}, sign, time.Hour, nil)
	h.svc = NewMFAService(MFADeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: h.sessions, Now: h.clock.now,
	})
	h.seedUser(100, "mfa@example.com")
	return h
}

func (h *mfaHarness) seedUser(id int64, email string) {
	now := time.Unix(1_700_000_000, 0)
	h.db.mu.Lock()
	defer h.db.mu.Unlock()
	h.db.users[id] = &User{
		ID: id, Email: email, Name: "MFA User", Status: UserStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	h.db.memberships[[2]int64{1, id}] = &Membership{
		ID: h.db.id(), TenantID: 1, UserID: id, Role: RoleMember,
		Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	h.db.passwords[id] = "hash"
}

// enrollAndConfirm returns the secret and the plaintext recovery codes.
func (h *mfaHarness) enrollAndConfirm(t *testing.T) (secret string, codes []string) {
	t.Helper()
	ctx := context.Background()
	enrollment, err := h.svc.EnrollTOTP(ctx, 100)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret = secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	codes, err = h.svc.ConfirmTOTP(ctx, 100, 1, code)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(codes))
	}
	return secret, codes
}

func secretFromURI(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse otpauth uri: %v", err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("unexpected otpauth uri %q", uri)
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		t.Fatal("otpauth uri has no secret")
	}
	return secret
}

func errorCode(err error) string {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func TestMFAEnrollConfirmAndTOTPLogin(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	secret, _ := h.enrollAndConfirm(t)

	status, err := h.svc.Status(ctx, 100)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Enabled || status.RecoveryCodesRemaining != 10 {
		t.Fatalf("status = %+v, want enabled with 10 codes", status)
	}
	if _, err := h.svc.EnrollTOTP(ctx, 100); errorCode(err) != string(identitycatalog.CodeAuthMFAAlreadyEnabled) {
		t.Fatalf("second enroll err = %v, want mfa_already_enabled", err)
	}

	// Confirmation consumed the first step; a new step starts the session.
	h.clock.at = h.clock.at.Add(totp.Period)
	challenge, err := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	login, err := h.svc.Verify(ctx, challenge, code)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if login.Tokens.AccessToken == "" || login.Tokens.RefreshToken == "" {
		t.Fatal("Verify returned no token pair")
	}
	if got := h.claims[len(h.claims)-1].AMR; !containsAll(got, AMRPassword, AMROTP) {
		t.Fatalf("access token amr = %v, want pwd+otp", got)
	}
	if login.Member.User.ID != 100 {
		t.Fatalf("member user = %d, want 100", login.Member.User.ID)
	}

	// The challenge is single use: a fresh, valid code cannot reuse it.
	next := h.clock.at.Add(totp.Period)
	nextCode, err := totp.Code(secret, next)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	h.clock.at = next
	if _, err := h.svc.Verify(ctx, challenge, nextCode); errorCode(err) != string(identitycatalog.CodeAuthMFAChallengeInvalid) {
		t.Fatalf("reused challenge err = %v, want mfa_challenge_invalid", err)
	}

	// The same TOTP step is rejected on replay even with a fresh challenge.
	challenge, err = h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	if _, err := h.svc.Verify(ctx, challenge, code); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("replayed code err = %v, want mfa_invalid_code", err)
	}
}

func TestMFARecoveryCodeIsSingleUse(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, _ := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	login, err := h.svc.Verify(ctx, challenge, codes[0])
	if err != nil {
		t.Fatalf("Verify recovery code: %v", err)
	}
	if got := h.claims[len(h.claims)-1].AMR; !containsAll(got, AMRPassword, AMRRecoveryCode) {
		t.Fatalf("access token amr = %v, want pwd+rc", got)
	}
	if login.Tokens.AccessToken == "" {
		t.Fatal("recovery-code login returned no access token")
	}

	challenge, _ = h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, codes[0]); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("reused recovery code err = %v, want mfa_invalid_code", err)
	}

	status, _ := h.svc.Status(ctx, 100)
	if status.RecoveryCodesRemaining != 9 {
		t.Fatalf("remaining recovery codes = %d, want 9", status.RecoveryCodesRemaining)
	}
	challenge, _ = h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, codes[1]); err != nil {
		t.Fatalf("second recovery code: %v", err)
	}
}

func TestMFAChallengeExpires(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	h.clock.at = h.clock.at.Add(mfaChallengeTTL + time.Second)
	if _, err := h.svc.Verify(ctx, challenge, codes[0]); errorCode(err) != string(identitycatalog.CodeAuthMFAChallengeInvalid) {
		t.Fatalf("expired challenge err = %v, want mfa_challenge_invalid", err)
	}
}

func TestMFAVerifyFailuresCountTowardLockout(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	for i := range maxMFAAttempts {
		_, err := h.svc.Verify(ctx, challenge, "000000")
		if statusOf(err) != 401 {
			t.Fatalf("attempt %d: err = %v, want 401", i+1, err)
		}
	}
	if _, err := h.svc.Verify(ctx, challenge, codes[0]); statusOf(err) != 429 {
		t.Fatalf("attempt after lockout err = %v, want 429", err)
	}
}

func TestMFADisableRevokesOtherSessions(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	secret, _ := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	current, err := h.sessions.Issue(ctx, 100, 0, []string{AMRPassword})
	if err != nil {
		t.Fatalf("Issue current: %v", err)
	}
	other, err := h.sessions.Issue(ctx, 100, 0, []string{AMRPassword})
	if err != nil {
		t.Fatalf("Issue other: %v", err)
	}

	code, _ := totp.Code(secret, h.clock.at)
	if err := h.svc.Disable(ctx, 100, current.SessionID, code); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if h.db.sessions[other.SessionID].RevokedAt == nil {
		t.Fatal("other session was not revoked")
	}
	if h.db.sessions[current.SessionID].RevokedAt != nil {
		t.Fatal("current session must stay live")
	}
	status, _ := h.svc.Status(ctx, 100)
	if status.Enabled {
		t.Fatal("MFA still enabled after disable")
	}
}

func TestMFARegenerateRecoveryCodes(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	newCodes, err := h.svc.RegenerateRecoveryCodes(ctx, 100, codes[0])
	if err != nil {
		t.Fatalf("RegenerateRecoveryCodes: %v", err)
	}
	if len(newCodes) != 10 {
		t.Fatalf("new codes = %d, want 10", len(newCodes))
	}
	if newCodes[0] == codes[0] {
		t.Fatal("regenerate returned an old code")
	}

	challenge, _ := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, codes[1]); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("old recovery code err = %v, want mfa_invalid_code", err)
	}
	challenge, _ = h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, newCodes[0]); err != nil {
		t.Fatalf("new recovery code: %v", err)
	}
}

func TestMFAConfirmRejectsInvalidCode(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	if _, err := h.svc.EnrollTOTP(ctx, 100); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, 100, 1, "000000"); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("confirm invalid code err = %v, want mfa_invalid_code", err)
	}
}

func TestBeginChallengeBindsTenant(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	// The same challenge is not valid in another tenant.
	if _, err := h.svc.Verify(tenant.WithID(ctx, 2), challenge, codes[0]); errorCode(err) != string(identitycatalog.CodeAuthMFAChallengeInvalid) {
		t.Fatalf("cross-tenant challenge err = %v, want mfa_challenge_invalid", err)
	}
}

func containsAll(values []string, want ...string) bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func TestGoogleSignInTwoStep(t *testing.T) {
	db := newMemDB()
	clock := &mfaClock{at: time.Unix(1_700_000_000, 0)}
	var signed []AccessClaims
	sign := func(c AccessClaims) (string, error) {
		signed = append(signed, c)
		return "access", nil
	}
	sessions := NewSessionService(db.stores(), memTx{db}, sign, time.Hour, nil)
	users := NewUserService(UserDeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache(), Sessions: sessions,
	})
	mfa := NewMFAService(MFADeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: sessions, Now: clock.now,
	})
	ctx := context.Background()
	req := GoogleSignIn{
		GoogleID: "google-subject", Email: "google@example.com", Name: "Google User",
		EmailVerified: true, AllowSignUp: true,
	}

	// The first sign-in creates the Google account and issues tokens.
	first, err := users.SignInWithGoogle(ctx, req)
	if err != nil {
		t.Fatalf("SignInWithGoogle: %v", err)
	}
	if first.MFARequired || first.Tokens.AccessToken == "" {
		t.Fatalf("first sign-in = %+v, want tokens", first)
	}

	// Enroll and confirm a TOTP factor for the Google account.
	enrollment, err := mfa.EnrollTOTP(ctx, first.Member.User.ID)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := mfa.ConfirmTOTP(ctx, first.Member.User.ID, 0, code); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	// The next Google sign-in is only the first factor.
	req.MFARequired = func(ctx context.Context, member *Member) (bool, error) {
		return mfa.Required(ctx, member.User.ID)
	}
	second, err := users.SignInWithGoogle(ctx, req)
	if err != nil {
		t.Fatalf("SignInWithGoogle with MFA: %v", err)
	}
	if !second.MFARequired || second.Tokens.AccessToken != "" || second.Tokens.RefreshToken != "" {
		t.Fatalf("MFA-required result = %+v, want no tokens", second)
	}

	clock.at = clock.at.Add(totp.Period)
	challenge, err := mfa.BeginChallenge(ctx, second.Member.User.ID, AMRGoogle)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	code, err = totp.Code(secret, clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	login, err := mfa.Verify(ctx, challenge, code)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if login.Tokens.AccessToken == "" || login.Tokens.RefreshToken == "" {
		t.Fatal("two-step Google login returned no token pair")
	}
	if got := signed[len(signed)-1].AMR; !containsAll(got, AMRGoogle, AMROTP) {
		t.Fatalf("amr = %v, want google+otp", got)
	}
}

func TestMFAConfirmRevokesOtherSessions(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	current, err := h.sessions.Issue(ctx, 100, 0, []string{AMRPassword})
	if err != nil {
		t.Fatalf("Issue current: %v", err)
	}
	other, err := h.sessions.Issue(ctx, 100, 0, []string{AMRPassword})
	if err != nil {
		t.Fatalf("Issue other: %v", err)
	}

	enrollment, err := h.svc.EnrollTOTP(ctx, 100)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, 100, current.SessionID, code); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if h.db.sessions[other.SessionID].RevokedAt == nil {
		t.Fatal("other session was not revoked")
	}
	if h.db.sessions[current.SessionID].RevokedAt != nil {
		t.Fatal("current session must stay live")
	}
}

func TestMFAChallengeVoidedByTokenVersionBump(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	secret, _ := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	// A password change or logout-all between the two steps bumps the version.
	h.db.mu.Lock()
	h.db.users[100].TokenVersion++
	h.db.mu.Unlock()

	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.Verify(ctx, challenge, code); err == nil {
		t.Fatal("Verify after a token version bump succeeded, want refusal")
	}
}

func TestMFABeginChallengeRequiresCompareAndDelete(t *testing.T) {
	h := newMFAHarness(t)
	svc := NewMFAService(MFADeps{
		Stores: h.db.stores(), Tx: memTx{h.db}, Cache: plainCache{cache.NewMemoryCache()},
		Sealer: reversibleSealer{}, Sessions: h.sessions, Now: h.clock.now,
	})
	_, err := svc.BeginChallenge(context.Background(), 100, AMRPassword)
	if errorCode(err) != string(apperror.CodeSystemUnavailable) {
		t.Fatalf("BeginChallenge err = %v, want system.unavailable", err)
	}
}

// plainCache hides CompareAndDelete from a cache.
type plainCache struct{ cache.Cache }

// A correct password clears the password counters; it must not reset the
// second-factor budget, or a caller who knows the password could keep
// guessing TOTP codes by logging in again between guesses.
func TestMFABudgetSurvivesPasswordSuccess(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)
	store := h.svc.(*mfaService).Cache

	challenge, err := h.svc.BeginChallenge(ctx, 100, AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	for i := range maxMFAAttempts {
		if i == maxMFAAttempts/2 {
			clearLoginAttempts(ctx, store, "mfa@example.com") // what a password login does
		}
		if _, err := h.svc.Verify(ctx, challenge, "000000"); statusOf(err) != 401 {
			t.Fatalf("attempt %d: err = %v, want 401", i+1, err)
		}
	}
	if _, err := h.svc.Verify(ctx, challenge, "000000"); statusOf(err) != 429 {
		t.Fatalf("attempt after the budget err = %v, want 429", err)
	}
}
