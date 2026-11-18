package mfa

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/softauthenticator"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const assuranceTestMaxAge = 12 * time.Hour

// issueProvenSession opens a session that records method, with its second
// factor verified at verifiedAt.
func (h *passkeyHarness) issueProvenSession(t *testing.T, userID uuid.UUID, method string, verifiedAt time.Time) uuid.UUID {
	t.Helper()
	pair, err := h.sessions.Issue(fakes.TenantCtx(), userID, 0, []string{core.AMRPassword, method})
	if err != nil {
		t.Fatalf("Issue session: %v", err)
	}
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	h.db.Sessions[pair.SessionID].MFAVerifiedAt = &verifiedAt
	return pair.SessionID
}

func (h *passkeyHarness) confirmTOTP(userID uuid.UUID) {
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	confirmedAt := h.clock.at
	h.db.Factors[userID] = &core.MFAFactor{ID: h.db.ID(), UserID: userID, ConfirmedAt: &confirmedAt}
}

func mfaRequired(err error) bool {
	return errorCode(err) == string(apperror.CodeMFARequired)
}

func TestEnsureFactorChangeAssurance(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fresh := now.Add(-time.Minute)
	stale := now.Add(-assuranceTestMaxAge - time.Second)
	future := now.Add(time.Minute)
	session := func(verifiedAt *time.Time, amr ...string) *Session {
		return &Session{AMR: amr, MFAVerifiedAt: verifiedAt}
	}
	cases := []struct {
		name     string
		enrolled enrolledFactor
		session  *Session
		allowed  bool
	}{
		{"no factor: a first-factor session bootstraps", enrolledNone, session(nil, core.AMRPassword), true},
		{"totp: first-factor session", enrolledTOTP, session(nil, core.AMRPassword), false},
		{"totp: fresh otp", enrolledTOTP, session(&fresh, core.AMRPassword, core.AMROTP), true},
		{"totp: fresh recovery code", enrolledTOTP, session(&fresh, core.AMRPassword, core.AMRRecoveryCode), true},
		{"totp: fresh passkey", enrolledTOTP, session(&fresh, core.PasskeyMethodHardware), true},
		{"totp: stale otp", enrolledTOTP, session(&stale, core.AMRPassword, core.AMROTP), false},
		{"totp: at the age bound", enrolledTOTP, session(ptrTime(now.Add(-assuranceTestMaxAge)), core.AMROTP), true},
		{"totp: verification in the future", enrolledTOTP, session(&future, core.AMROTP), false},
		{"totp: method without a verification time", enrolledTOTP, session(nil, core.AMROTP), false},
		{"passkey: fresh otp is not enough", enrolledPasskey, session(&fresh, core.AMRPassword, core.AMROTP), false},
		{"passkey: fresh hardware key", enrolledPasskey, session(&fresh, core.PasskeyMethodHardware), true},
		{"passkey: fresh synced key", enrolledPasskey, session(&fresh, core.PasskeyMethodSoftware), true},
		{"passkey: stale key", enrolledPasskey, session(&stale, core.PasskeyMethodHardware), false},
		{"passkey: first-factor session", enrolledPasskey, session(nil, core.AMRPassword), false},
		{"unknown state fails closed", enrolledFactor(99), session(&fresh, core.PasskeyMethodHardware), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ensureFactorChangeAssurance(tc.enrolled, tc.session, now, assuranceTestMaxAge)
			if tc.allowed && err != nil {
				t.Fatalf("err = %v, want allowed", err)
			}
			if !tc.allowed && err == nil {
				t.Fatal("allowed, want a refusal")
			}
		})
	}
}

func ptrTime(at time.Time) *time.Time { return &at }

// A stolen password must not be enough to add a passkey to an account that
// already has one: the new credential would satisfy the passkey-only gate.
func TestPasskeyRegistrationNeedsAPasskeySessionOnceOneExists(t *testing.T) {
	h := newPasskeyHarness(t)
	h.register(t, softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin))
	ctx, user := fakes.TenantCtx(), fakes.TestID(200)

	_, err := h.svc.BeginRegistration(ctx, user, ReauthProof{SessionID: h.issueSession(t, user)})
	if !mfaRequired(err) {
		t.Fatalf("first-factor session err = %v, want auth.mfa_required", err)
	}
	_, err = h.svc.BeginRegistration(ctx, user, ReauthProof{
		SessionID: h.issueProvenSession(t, user, core.AMROTP, h.clock.at),
	})
	if !mfaRequired(err) {
		t.Fatalf("otp session err = %v, want auth.mfa_required", err)
	}
	_, err = h.svc.BeginRegistration(ctx, user, ReauthProof{
		SessionID: h.issueProvenSession(t, user, core.PasskeyMethodHardware, h.clock.at.Add(-assuranceTestMaxAge-time.Second)),
	})
	if !mfaRequired(err) {
		t.Fatalf("stale passkey session err = %v, want auth.mfa_required", err)
	}
	if _, err := h.svc.BeginRegistration(ctx, user, ReauthProof{
		SessionID: h.issueProvenSession(t, user, core.PasskeyMethodHardware, h.clock.at),
	}); err != nil {
		t.Fatalf("fresh passkey session: %v", err)
	}
}

func TestPasskeyRegistrationNeedsAFreshSecondFactorWhenTOTPIsEnrolled(t *testing.T) {
	h := newPasskeyHarness(t)
	ctx, user := fakes.TenantCtx(), fakes.TestID(200)
	firstFactorSession := h.issueSession(t, user)
	h.confirmTOTP(user)

	if _, err := h.svc.BeginRegistration(ctx, user, ReauthProof{SessionID: firstFactorSession}); !mfaRequired(err) {
		t.Fatalf("first-factor session err = %v, want auth.mfa_required", err)
	}
	if _, err := h.svc.BeginRegistration(ctx, user, ReauthProof{
		SessionID: h.issueProvenSession(t, user, core.AMROTP, h.clock.at),
	}); err != nil {
		t.Fatalf("fresh otp session: %v", err)
	}
}

// The ceremony lasts five minutes. The session that started it can lose its
// proof in that time (a TOTP step-up removes the passkey method), so the rule
// is checked again in the transaction that stores the credential.
func TestPasskeyFinishRegistrationRechecksTheAssurance(t *testing.T) {
	h := newPasskeyHarness(t)
	h.register(t, softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin))
	ctx, user := fakes.TenantCtx(), fakes.TestID(200)
	sessionID := h.issueProvenSession(t, user, core.PasskeyMethodHardware, h.clock.at)
	creation, err := h.svc.BeginRegistration(ctx, user, ReauthProof{SessionID: sessionID})
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	h.db.Mu.Lock()
	h.db.Sessions[sessionID].AMR = []string{core.AMRPassword, core.AMROTP}
	h.db.Mu.Unlock()

	second := softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin)
	_, err = h.svc.FinishRegistration(ctx, user, "Second", second.Attestation(t, creation.Response.Challenge.String()))
	if !mfaRequired(err) {
		t.Fatalf("FinishRegistration err = %v, want auth.mfa_required", err)
	}
	passkeys, err := h.svc.List(ctx, user)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(passkeys) != 1 {
		t.Fatalf("passkey count = %d, want the refused registration to store nothing", len(passkeys))
	}
}

// Deleting the passkeys would downgrade the account to a state where a weaker
// proof can enroll a new one, so deletion follows the same rule.
func TestPasskeyDeleteNeedsAPasskeySession(t *testing.T) {
	h := newPasskeyHarness(t)
	passkey := h.register(t, softauthenticator.New(t, passkeyTestRPID, passkeyTestOrigin))
	ctx, user := fakes.TenantCtx(), fakes.TestID(200)

	err := h.svc.Delete(ctx, user, passkey.ID, ReauthProof{SessionID: h.issueSession(t, user)})
	if !mfaRequired(err) {
		t.Fatalf("first-factor session err = %v, want auth.mfa_required", err)
	}
	err = h.svc.Delete(ctx, user, passkey.ID, ReauthProof{
		SessionID: h.issueProvenSession(t, user, core.AMROTP, h.clock.at),
	})
	if !mfaRequired(err) {
		t.Fatalf("otp session err = %v, want auth.mfa_required", err)
	}
	if err := h.svc.Delete(ctx, user, passkey.ID, ReauthProof{
		SessionID: h.issueProvenSession(t, user, core.PasskeyMethodHardware, h.clock.at),
	}); err != nil {
		t.Fatalf("fresh passkey session: %v", err)
	}
}

// A TOTP or recovery-code step-up proves a phishable factor. It must not
// leave an earlier passkey method on the session, or it would refresh the
// passkey-only gate of every platform:* operation.
func TestMFAStepUpWithACodeRemovesThePasskeyMethod(t *testing.T) {
	h := newMFAHarness(t)
	_, codes := h.enrollAndConfirm(t)
	session := h.issueSession(t, []string{core.AMRPassword, core.PasskeyMethodHardware, core.PasskeyMethodSoftware})

	if _, err := h.svc.(MFAStepUpService).StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, codes[0]); err != nil {
		t.Fatalf("StepUp: %v", err)
	}
	h.db.Mu.Lock()
	amr := append([]string(nil), h.db.Sessions[session.ID].AMR...)
	h.db.Mu.Unlock()
	if hasAMR(amr, core.PasskeyMethodHardware) || hasAMR(amr, core.PasskeyMethodSoftware) {
		t.Fatalf("session amr = %v, want no passkey method after a code step-up", amr)
	}
	if !hasAMR(amr, core.AMRPassword) || !hasAMR(amr, core.AMRRecoveryCode) {
		t.Fatalf("session amr = %v, want the first factor and the recovery-code method", amr)
	}
}

// The platform reset is the recovery path for a user who lost every factor.
// It must remove passkeys too: a lost or stolen authenticator would otherwise
// still sign in after the reset.
func TestMFAResetTOTPDeletesPasskeys(t *testing.T) {
	h := newMFAHarness(t)
	h.enrollAndConfirm(t)
	h.db.Mu.Lock()
	h.db.Passkeys[fakes.TestID(900)] = &core.Passkey{ID: fakes.TestID(900), UserID: fakes.TestID(100), Name: "Lost key"}
	h.db.Mu.Unlock()

	if err := h.svc.ResetTOTP(fakes.TenantCtx(), fakes.TestID(100)); err != nil {
		t.Fatalf("ResetTOTP: %v", err)
	}
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	for _, passkey := range h.db.Passkeys {
		if passkey.UserID == fakes.TestID(100) {
			t.Fatalf("passkey %s survived the reset", passkey.ID)
		}
	}
}
