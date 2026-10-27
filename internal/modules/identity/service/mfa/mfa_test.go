package mfa

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
	"github.com/google/uuid"
)

// testMFAPassword is the local password of the MFA harness user.
var testMFAPassword = fakes.Passphrase

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
	db       *fakes.MemDB
	svc      MFAService
	sessions *authn.SessionService
	clock    *mfaClock
	claims   []authn.AccessClaims
}

func newMFAHarness(t *testing.T) *mfaHarness {
	t.Helper()
	db := fakes.New()
	return newMFAHarnessWithTx(t, db, fakes.Tx{DB: db})
}

func newMFAHarnessWithTx(t *testing.T, db *fakes.MemDB, tx core.Transactor) *mfaHarness {
	t.Helper()
	h := &mfaHarness{db: db, clock: &mfaClock{at: time.Unix(1_700_000_000, 0)}}

	sign := func(c authn.AccessClaims) (string, error) {
		h.claims = append(h.claims, c)
		return token.GenerateForSession(token.SessionClaims{
			UserID: c.UserID, TenantID: c.TenantID, SessionID: c.SessionID,
			TokenVersion: c.TokenVersion, AMR: c.AMR,
		}, "mfa-test-jwt-secret-0000000000000", time.Hour)
	}
	h.sessions = authn.NewSessionService(db.Stores(), tx, sign, time.Hour, 720*time.Hour, nil)
	h.svc = NewMFAService(MFADeps{
		Stores: db.Stores(), Tx: tx, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: h.sessions, Now: h.clock.now,
	})
	h.seedUser(fakes.TestID(100), "mfa@example.com")
	return h
}

func (h *mfaHarness) seedUser(id uuid.UUID, email string) {
	now := time.Unix(1_700_000_000, 0)
	hash, err := core.HashPassword(testMFAPassword)
	if err != nil {
		panic(err)
	}
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	h.db.Users[id] = &core.User{
		ID: id, Email: email, Name: "MFA User", Status: core.UserStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	h.db.Memberships[[2]uuid.UUID{fakes.TestID(1), id}] = &core.Membership{
		ID: h.db.ID(), TenantID: fakes.TestID(1), UserID: id, Role: core.RoleMember,
		Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	h.db.Passwords[id] = hash
}

// enrollAndConfirm returns the secret and the plaintext recovery codes.
func (h *mfaHarness) enrollAndConfirm(t *testing.T) (secret string, codes []string) {
	t.Helper()
	ctx := fakes.TenantCtx()
	enrollment, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret = secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	result, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword, code)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if len(result.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(result.RecoveryCodes))
	}
	return secret, result.RecoveryCodes
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

func TestMFAEnrollRequiresCurrentPassword(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()

	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), ""); statusOf(err) != 400 {
		t.Fatalf("enroll without a password err = %v, want 400", err)
	}
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), fakes.WrongPassword); statusOf(err) != 400 {
		t.Fatalf("enroll with a wrong password err = %v, want 400", err)
	}
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword); err != nil {
		t.Fatalf("enroll with the current password: %v", err)
	}
}

// A wrong current password spends the per-user second-factor budget, so a
// stolen access token cannot brute-force the password through enroll.
func TestMFAEnrollWrongPasswordSpendsSecondFactorBudget(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	for i := range MaxMFAAttempts {
		if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), fakes.WrongPassword); statusOf(err) != 400 {
			t.Fatalf("attempt %d: err = %v, want 400", i+1, err)
		}
	}
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword); statusOf(err) != 429 {
		t.Fatalf("enroll after the budget err = %v, want 429", err)
	}
}

// A passwordless account must have signed in within the re-authentication
// window; an old session (or no session) is refused.
func TestMFAEnrollPasswordlessRequiresRecentSession(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	h.db.Mu.Lock()
	delete(h.db.Passwords, fakes.TestID(100))
	h.db.Mu.Unlock()

	old, err := h.sessions.Issue(ctx, fakes.TestID(100), 0, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue old session: %v", err)
	}
	h.db.Mu.Lock()
	h.db.Sessions[old.SessionID].CreatedAt = h.clock.at.Add(-mfaReauthSessionMaxAge - time.Minute)
	h.db.Mu.Unlock()
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), old.SessionID, ""); errorCode(err) != string(identitycatalog.CodeAuthMFAReauthRequired) {
		t.Fatalf("old session err = %v, want mfa_reauth_required", err)
	}

	fresh, err := h.sessions.Issue(ctx, fakes.TestID(100), 0, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue fresh session: %v", err)
	}
	h.db.Mu.Lock()
	h.db.Sessions[fresh.SessionID].CreatedAt = h.clock.at
	h.db.Mu.Unlock()
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fresh.SessionID, ""); err != nil {
		t.Fatalf("fresh session enroll: %v", err)
	}
}

// Re-enrolling replaces the pending secret; only the newest URI can confirm.
func TestMFAEnrollReplacesPendingSecret(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	first, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	firstSecret := secretFromURI(t, first.OtpauthURI)

	h.clock.at = h.clock.at.Add(time.Minute)
	second, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword)
	if err != nil {
		t.Fatalf("second enroll: %v", err)
	}
	secondSecret := secretFromURI(t, second.OtpauthURI)
	if firstSecret == secondSecret {
		t.Fatal("re-enrollment reused the pending secret")
	}

	firstCode, err := totp.Code(firstSecret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword, firstCode); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("confirm with replaced secret err = %v, want mfa_invalid_code", err)
	}
	secondCode, err := totp.Code(secondSecret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword, secondCode); err != nil {
		t.Fatalf("confirm with the current secret: %v", err)
	}
}

func TestMFAConfirmRejectsExpiredEnrollment(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	enrollment, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	h.db.Mu.Lock()
	h.db.Factors[fakes.TestID(100)].UpdatedAt = h.clock.at.Add(-mfaEnrollmentTTL - time.Second)
	h.db.Mu.Unlock()

	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword, code); errorCode(err) != string(identitycatalog.CodeAuthMFAEnrollmentExpired) {
		t.Fatalf("expired enrollment err = %v, want mfa_enrollment_expired", err)
	}
}

func TestMFAConfirmRequiresCurrentPassword(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	enrollment, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), fakes.TestID(1), fakes.WrongPassword, code); statusOf(err) != 400 {
		t.Fatalf("confirm with a wrong password err = %v, want 400", err)
	}
}

// A password change removes a pending enrollment in the same transaction, so
// an enrollment started before the change cannot be confirmed afterwards.
func TestPasswordChangeDeletesPendingMFA(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	users := authn.NewUserService(authn.UserDeps{
		Stores: h.db.Stores(), Tx: fakes.Tx{DB: h.db}, Cache: cache.NewMemoryCache(),
	})
	if err := users.ChangePassword(ctx, fakes.TestID(100), authn.ChangePasswordInput{
		CurrentPassword: testMFAPassword, NewPassword: fakes.NewPassword,
	}); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, ok := h.db.Factors[fakes.TestID(100)]; ok {
		t.Fatal("password change left the pending MFA enrollment")
	}
}

// The platform reset removes the factor, recovery codes, all sessions, and
// bumps the token version, and records a Tier 1 audit event.
func TestMFAResetTOTP(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	_, _ = h.enrollAndConfirm(t)
	current, err := h.sessions.Issue(ctx, fakes.TestID(100), h.db.Users[fakes.TestID(100)].TokenVersion, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue session: %v", err)
	}
	version := h.db.Users[fakes.TestID(100)].TokenVersion

	if err := h.svc.ResetTOTP(ctx, fakes.TestID(100)); err != nil {
		t.Fatalf("ResetTOTP: %v", err)
	}
	if _, ok := h.db.Factors[fakes.TestID(100)]; ok {
		t.Fatal("reset left the MFA factor")
	}
	if len(h.db.Recovery[fakes.TestID(100)]) != 0 {
		t.Fatal("reset left recovery codes")
	}
	if h.db.Sessions[current.SessionID].RevokedAt == nil {
		t.Fatal("reset left the user's session live")
	}
	if h.db.Users[fakes.TestID(100)].TokenVersion <= version {
		t.Fatal("reset did not bump the token version")
	}
	found := false
	for _, event := range h.db.Audits {
		if event.Action == "admin.user.mfa_reset" {
			found = true
		}
	}
	if !found {
		t.Fatal("reset did not write its audit event")
	}
}

func TestMFAResetUnknownUser(t *testing.T) {
	h := newMFAHarness(t)
	if err := h.svc.ResetTOTP(fakes.TenantCtx(), fakes.TestID(999)); statusOf(err) != 404 {
		t.Fatalf("reset unknown user err = %v, want 404", err)
	}
}

func TestMFAEnrollConfirmAndTOTPLogin(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	secret, _ := h.enrollAndConfirm(t)

	status, err := h.svc.Status(ctx, fakes.TestID(100))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Enabled || status.RecoveryCodesRemaining != 10 {
		t.Fatalf("status = %+v, want enabled with 10 codes", status)
	}
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword); errorCode(err) != string(identitycatalog.CodeAuthMFAAlreadyEnabled) {
		t.Fatalf("second enroll err = %v, want mfa_already_enabled", err)
	}

	// Confirmation consumed the first step; a new step starts the session.
	h.clock.at = h.clock.at.Add(totp.Period)
	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
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
	if got := h.claims[len(h.claims)-1].AMR; !containsAll(got, core.AMRPassword, core.AMROTP) {
		t.Fatalf("access token amr = %v, want pwd+otp", got)
	}
	if login.Member.User.ID != fakes.TestID(100) {
		t.Fatalf("member user = %s, want fakes.TestID(100)", login.Member.User.ID)
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
	challenge, err = h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	if _, err := h.svc.Verify(ctx, challenge, code); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("replayed code err = %v, want mfa_invalid_code", err)
	}
}

func TestMFARecoveryCodeIsSingleUse(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, _ := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	login, err := h.svc.Verify(ctx, challenge, codes[0])
	if err != nil {
		t.Fatalf("Verify recovery code: %v", err)
	}
	if got := h.claims[len(h.claims)-1].AMR; !containsAll(got, core.AMRPassword, core.AMRRecoveryCode) {
		t.Fatalf("access token amr = %v, want pwd+rc", got)
	}
	if login.Tokens.AccessToken == "" {
		t.Fatal("recovery-code login returned no access token")
	}

	challenge, _ = h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, codes[0]); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("reused recovery code err = %v, want mfa_invalid_code", err)
	}

	status, _ := h.svc.Status(ctx, fakes.TestID(100))
	if status.RecoveryCodesRemaining != 9 {
		t.Fatalf("remaining recovery codes = %d, want 9", status.RecoveryCodesRemaining)
	}
	challenge, _ = h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, codes[1]); err != nil {
		t.Fatalf("second recovery code: %v", err)
	}
}

func TestMFAChallengeExpires(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
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
	ctx := fakes.TenantCtx()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	for i := range MaxMFAAttempts {
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
	ctx := fakes.TenantCtx()
	secret, _ := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	current, err := h.sessions.Issue(ctx, fakes.TestID(100), h.db.Users[fakes.TestID(100)].TokenVersion, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue current: %v", err)
	}
	other, err := h.sessions.Issue(ctx, fakes.TestID(100), h.db.Users[fakes.TestID(100)].TokenVersion, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue other: %v", err)
	}

	code, _ := totp.Code(secret, h.clock.at)
	if err := h.svc.Disable(ctx, fakes.TestID(100), current.SessionID, code); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if h.db.Sessions[other.SessionID].RevokedAt == nil {
		t.Fatal("other session was not revoked")
	}
	if h.db.Sessions[current.SessionID].RevokedAt != nil {
		t.Fatal("current session must stay live")
	}
	status, _ := h.svc.Status(ctx, fakes.TestID(100))
	if status.Enabled {
		t.Fatal("MFA still enabled after disable")
	}
}

func TestMFARegenerateRecoveryCodes(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	newCodes, err := h.svc.RegenerateRecoveryCodes(ctx, fakes.TestID(100), codes[0])
	if err != nil {
		t.Fatalf("RegenerateRecoveryCodes: %v", err)
	}
	if len(newCodes) != 10 {
		t.Fatalf("new codes = %d, want 10", len(newCodes))
	}
	if newCodes[0] == codes[0] {
		t.Fatal("regenerate returned an old code")
	}

	challenge, _ := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, codes[1]); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("old recovery code err = %v, want mfa_invalid_code", err)
	}
	challenge, _ = h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if _, err := h.svc.Verify(ctx, challenge, newCodes[0]); err != nil {
		t.Fatalf("new recovery code: %v", err)
	}
}

func TestMFAConfirmRejectsInvalidCode(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	if _, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword, "000000"); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("confirm invalid code err = %v, want mfa_invalid_code", err)
	}
}

func TestBeginChallengeBindsTenant(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	_, codes := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	// The same challenge is not valid in another tenant.
	if _, err := h.svc.Verify(tenant.WithID(ctx, fakes.TestID(2)), challenge, codes[0]); errorCode(err) != string(identitycatalog.CodeAuthMFAChallengeInvalid) {
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
	db := fakes.New()
	clock := &mfaClock{at: time.Unix(1_700_000_000, 0)}
	var signed []authn.AccessClaims
	sign := func(c authn.AccessClaims) (string, error) {
		signed = append(signed, c)
		return "access", nil
	}
	sessions := authn.NewSessionService(db.Stores(), fakes.Tx{DB: db}, sign, time.Hour, 720*time.Hour, nil)
	users := authn.NewUserService(authn.UserDeps{
		Stores: db.Stores(), Tx: fakes.Tx{DB: db}, Cache: cache.NewMemoryCache(), Sessions: sessions,
	})
	mfa := NewMFAService(MFADeps{
		Stores: db.Stores(), Tx: fakes.Tx{DB: db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: sessions, Now: clock.now,
	})
	ctx := fakes.TenantCtx()
	req := authn.GoogleSignIn{
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
	enrollment, err := mfa.EnrollTOTP(ctx, first.Member.User.ID, first.Tokens.SessionID, "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := mfa.ConfirmTOTP(ctx, first.Member.User.ID, first.Tokens.SessionID, "", code); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	// The next Google sign-in is only the first factor.
	req.MFARequired = func(ctx context.Context, member *core.Member) (bool, error) {
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
	challenge, err := mfa.BeginChallenge(ctx, second.Member.User.ID, core.AMRGoogle)
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
	if got := signed[len(signed)-1].AMR; !containsAll(got, core.AMRGoogle, core.AMROTP) {
		t.Fatalf("amr = %v, want google+otp", got)
	}
}

func TestMFAConfirmRevokesOtherSessions(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	current, err := h.sessions.Issue(ctx, fakes.TestID(100), 0, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue current: %v", err)
	}
	other, err := h.sessions.Issue(ctx, fakes.TestID(100), 0, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue other: %v", err)
	}

	enrollment, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), fakes.TestID(1), testMFAPassword)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	if _, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), current.SessionID, testMFAPassword, code); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if h.db.Sessions[other.SessionID].RevokedAt == nil {
		t.Fatal("other session was not revoked")
	}
	if h.db.Sessions[current.SessionID].RevokedAt != nil {
		t.Fatal("current session must stay live")
	}
}

// Confirming a factor bumps the global token version, so a password-only
// session created by a login that read MFARequired=false before the
// confirmation can never be used, and re-issues the session that confirmed
// the factor under the new version so that caller stays signed in.
func TestMFAConfirmBumpsVersionAndKeepsCurrentSession(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	version := h.db.Users[fakes.TestID(100)].TokenVersion
	current, err := h.sessions.Issue(ctx, fakes.TestID(100), version, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue current: %v", err)
	}
	other, err := h.sessions.Issue(ctx, fakes.TestID(100), version, []string{core.AMRPassword})
	if err != nil {
		t.Fatalf("Issue other: %v", err)
	}

	enrollment, err := h.svc.EnrollTOTP(ctx, fakes.TestID(100), current.SessionID, testMFAPassword)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := secretFromURI(t, enrollment.OtpauthURI)
	code, err := totp.Code(secret, h.clock.at)
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	result, err := h.svc.ConfirmTOTP(ctx, fakes.TestID(100), current.SessionID, testMFAPassword, code)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	if got := h.db.Users[fakes.TestID(100)].TokenVersion; got != version+1 {
		t.Fatalf("token version = %d, want %d (confirmation must invalidate every older session)", got, version+1)
	}
	kept := h.db.Sessions[current.SessionID]
	if kept.RevokedAt != nil {
		t.Fatal("current session must stay live")
	}
	if kept.TokenVersion != h.db.Users[fakes.TestID(100)].TokenVersion {
		t.Fatalf("current session token version = %d, want %d", kept.TokenVersion, h.db.Users[fakes.TestID(100)].TokenVersion)
	}
	if !containsAll(kept.AMR, core.AMROTP) {
		t.Fatalf("current session amr = %v, want otp", kept.AMR)
	}
	if h.db.Sessions[other.SessionID].RevokedAt == nil {
		t.Fatal("other session was not revoked")
	}
	if result.Tokens.SessionID != current.SessionID || result.Tokens.AccessToken == "" || result.Tokens.RefreshToken == "" {
		t.Fatalf("re-issued pair = %+v, want a fresh pair for session %d", result.Tokens, current.SessionID)
	}
	if len(result.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(result.RecoveryCodes))
	}
}

func TestMFAChallengeVoidedByTokenVersionBump(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	secret, _ := h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	// A password change or logout-all between the two steps bumps the version.
	h.db.Mu.Lock()
	h.db.Users[fakes.TestID(100)].TokenVersion++
	h.db.Mu.Unlock()

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
		Stores: h.db.Stores(), Tx: fakes.Tx{DB: h.db}, Cache: plainCache{cache.NewMemoryCache()},
		Sealer: reversibleSealer{}, Sessions: h.sessions, Now: h.clock.now,
	})
	_, err := svc.BeginChallenge(fakes.TenantCtx(), fakes.TestID(100), core.AMRPassword)
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
	ctx := fakes.TenantCtx()
	h.enrollAndConfirm(t)
	h.clock.at = h.clock.at.Add(totp.Period)
	store := h.svc.(*mfaService).Cache

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	if err != nil {
		t.Fatalf("BeginChallenge: %v", err)
	}
	for i := range MaxMFAAttempts {
		if i == MaxMFAAttempts/2 {
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
