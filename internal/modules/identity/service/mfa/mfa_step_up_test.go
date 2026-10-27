package mfa

import (
	"context"
	"net/http"
	"testing"
	"time"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/google/uuid"
)

// issueSession opens a session for the MFA harness user and returns its row.
// The user has a confirmed factor, so (as in production, where CreateSession
// refuses a first-factor-only session for such a user) the session carries the
// otp method; its mfa_verified_at is then cleared to model a session whose
// second factor is too old, which is what step-up refreshes. The session is
// issued at the user's current token version, which MFA confirmation bumps.
func (h *mfaHarness) issueSession(t *testing.T, amr []string) *core.Session {
	t.Helper()
	h.db.Mu.Lock()
	version := h.db.Users[fakes.TestID(100)].TokenVersion
	h.db.Mu.Unlock()
	pair, err := h.sessions.Issue(tenant.WithID(context.Background(), tenant.DefaultID), fakes.TestID(100), version, append(amr, core.AMROTP))
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	session, ok := h.db.Sessions[pair.SessionID]
	if !ok {
		t.Fatalf("session %s not stored", pair.SessionID)
	}
	session.MFAVerifiedAt = nil
	copied := *session
	return &copied
}

func (h *mfaHarness) sessionMFAVerifiedAt(sessionID uuid.UUID) *time.Time {
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	session, ok := h.db.Sessions[sessionID]
	if !ok {
		return nil
	}
	if session.MFAVerifiedAt == nil {
		return nil
	}
	at := *session.MFAVerifiedAt
	return &at
}

func (h *mfaHarness) auditedActions() []string {
	h.db.Mu.Lock()
	defer h.db.Mu.Unlock()
	actions := make([]string, 0, len(h.db.Audits))
	for _, event := range h.db.Audits {
		actions = append(actions, event.Action)
	}
	return actions
}

func TestMFAStepUpMarksTheCallersSession(t *testing.T) {
	h := newMFAHarness(t)
	_, codes := h.enrollAndConfirm(t)
	session := h.issueSession(t, []string{core.AMRPassword})
	other := h.issueSession(t, []string{core.AMRPassword})

	verifiedAt, err := h.svc.(MFAStepUpService).StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, codes[0])
	if err != nil {
		t.Fatalf("StepUp: %v", err)
	}
	if verifiedAt.IsZero() {
		t.Fatal("StepUp returned the zero time")
	}
	stored := h.sessionMFAVerifiedAt(session.ID)
	if stored == nil || !stored.Equal(verifiedAt) {
		t.Fatalf("session mfa_verified_at = %v, want %v", stored, verifiedAt)
	}
	if got := h.sessionMFAVerifiedAt(other.ID); got != nil {
		t.Errorf("another session's mfa_verified_at = %v, want nil", got)
	}
	actions := h.auditedActions()
	if !containsAction(actions, "auth.mfa_step_up") {
		t.Errorf("audit actions = %v, want auth.mfa_step_up", actions)
	}
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

// A code is single use: the same recovery code cannot refresh two sessions.
func TestMFAStepUpConsumesTheCode(t *testing.T) {
	h := newMFAHarness(t)
	_, codes := h.enrollAndConfirm(t)
	session := h.issueSession(t, []string{core.AMRPassword})
	svc := h.svc.(MFAStepUpService)

	if _, err := svc.StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, codes[0]); err != nil {
		t.Fatalf("first StepUp: %v", err)
	}
	if _, err := svc.StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, codes[0]); errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("replayed code error = %v, want mfa_invalid_code", err)
	}
}

// A session that is not the caller's own (unknown, foreign, or revoked) makes
// the step-up fail closed and leaves the AAL2 clock untouched.
func TestMFAStepUpRejectsUnknownSession(t *testing.T) {
	h := newMFAHarness(t)
	_, codes := h.enrollAndConfirm(t)
	session := h.issueSession(t, []string{core.AMRPassword})
	svc := h.svc.(MFAStepUpService)

	_, err := svc.StepUp(fakes.TenantCtx(), fakes.TestID(100), fakes.TestID(9999), codes[0])
	if statusOf(err) != http.StatusUnauthorized {
		t.Fatalf("unknown session error = %v, want 401", err)
	}
	if got := h.sessionMFAVerifiedAt(session.ID); got != nil {
		t.Fatalf("session mfa_verified_at = %v after a failed step-up, want nil", got)
	}
	// A valid session can still step up (the production transaction rolls the
	// failed attempt back; the in-memory fake does not model rollbacks, so it
	// uses a different single-use code here).
	if _, err := svc.StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, codes[1]); err != nil {
		t.Fatalf("StepUp on a valid session: %v", err)
	}
}

// A wrong code must not refresh the AAL2 clock.
func TestMFAStepUpRejectsInvalidCode(t *testing.T) {
	h := newMFAHarness(t)
	h.enrollAndConfirm(t)
	session := h.issueSession(t, []string{core.AMRPassword})

	_, err := h.svc.(MFAStepUpService).StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, "000000")
	if errorCode(err) != string(identitycatalog.CodeAuthMFAInvalidCode) {
		t.Fatalf("invalid code error = %v, want mfa_invalid_code", err)
	}
	if got := h.sessionMFAVerifiedAt(session.ID); got != nil {
		t.Fatalf("session mfa_verified_at = %v, want nil", got)
	}
}

// Step-up shares the per-user second-factor budget with login verification.
func TestMFAStepUpSharesTheSecondFactorBudget(t *testing.T) {
	h := newMFAHarness(t)
	h.enrollAndConfirm(t)
	session := h.issueSession(t, []string{core.AMRPassword})
	svc := h.svc.(MFAStepUpService)

	for i := range MaxMFAAttempts {
		_, err := svc.StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, "000000")
		if statusOf(err) != http.StatusUnauthorized {
			t.Fatalf("attempt %d error = %v, want 401", i+1, err)
		}
	}
	_, err := svc.StepUp(fakes.TenantCtx(), fakes.TestID(100), session.ID, "000000")
	if statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("attempt past the budget error = %v, want 429", err)
	}
}
