package mfa

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

func failures(events []audit.Event, action string) []audit.Event {
	var out []audit.Event
	for _, event := range events {
		if event.Action == action {
			out = append(out, event)
		}
	}
	return out
}

// A failed second-factor guess is audited even though the login request fails,
// attributed to the user and carrying only a reason class (never the code).
func TestMFAVerifyFailureIsAuditedInItsOwnTransaction(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	h.enrollAndConfirm(t)

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	require.NoError(t, err)
	const attempted = "not-a-real-code"
	_, err = h.svc.Verify(ctx, challenge, attempted)
	require.Error(t, err)
	assert.Equal(t, string(identitycatalog.CodeAuthMFAInvalidCode), errorCode(err))

	failed := failures(h.db.Audits, "auth.mfa_verify_failed")
	require.Len(t, failed, 1, "one failure audit for one failed guess")
	event := failed[0]
	assert.Equal(t, fakes.TestID(100), event.ActorID)
	assert.Equal(t, audit.ActorTypeUser, event.ActorType)
	assert.Equal(t, audit.OutcomeFailed, event.Outcome)
	assert.Equal(t, fakes.TestID(100).String(), event.ResourceID)
	assert.Equal(t, "invalid_code", event.Metadata["reason"])
	assert.NotContains(t, event.Metadata, "code", "the attempted code is never audit metadata")
	for _, value := range event.Metadata {
		assert.NotEqual(t, attempted, value)
	}
	assert.Empty(t, failures(h.db.Audits, "auth.mfa_login"))
}

// The attempt that exhausts the per-user budget records a lockout event once;
// later rejected attempts record only the ordinary failure.
func TestMFABudgetExhaustionAuditedOnce(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	h.enrollAndConfirm(t)
	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	require.NoError(t, err)

	for i := range MaxMFAAttempts + 2 {
		_, err := h.svc.Verify(ctx, challenge, "not-a-real-code")
		require.Error(t, err, "attempt %d", i+1)
	}
	assert.Len(t, failures(h.db.Audits, "auth.mfa_attempts_exhausted"), 1,
		"the exhaustion event is recorded once, on the attempt that crosses the limit")
	assert.Len(t, failures(h.db.Audits, "auth.mfa_verify_failed"), MaxMFAAttempts+2)

	var budgetReasons int
	for _, event := range failures(h.db.Audits, "auth.mfa_verify_failed") {
		if event.Metadata["reason"] == "budget_exhausted" {
			budgetReasons++
		}
	}
	assert.Equal(t, 2, budgetReasons, "the exhausting and the already-exhausted attempts are budget failures")
}

// Disable and regenerate also consume a second-factor guess, so their failed
// code proof is audited the same way.
func TestMFADisableAndRegenerateFailuresAreAudited(t *testing.T) {
	for name, call := range map[string]func(h *mfaHarness, ctx context.Context) error{
		"disable": func(h *mfaHarness, ctx context.Context) error {
			return h.svc.Disable(ctx, fakes.TestID(100), MFACodeChange{FactorID: h.factorID(t), SessionID: fakes.TestID(1), CurrentPassword: testMFAPassword, Code: "not-a-real-code"})
		},
		"regenerate": func(h *mfaHarness, ctx context.Context) error {
			_, err := h.svc.RegenerateRecoveryCodes(ctx, fakes.TestID(100), MFARecoveryChange{SessionID: fakes.TestID(1), CurrentPassword: testMFAPassword, Code: "not-a-real-code"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newMFAHarness(t)
			ctx := fakes.TenantCtx()
			h.enrollAndConfirm(t)
			err := call(h, ctx)
			require.Error(t, err)
			assert.Equal(t, string(identitycatalog.CodeAuthMFAInvalidCode), errorCode(err))
			failed := failures(h.db.Audits, "auth.mfa_verify_failed")
			require.Len(t, failed, 1)
			assert.Equal(t, "invalid_code", failed[0].Metadata["reason"])
		})
	}
}

// failingMFAFailureAudit fails only the failure/lockout rows, so setup can
// still audit enrollment and confirmation.
type failingMFAFailureAudit struct{ db *fakes.MemDB }

func (a failingMFAFailureAudit) RecordTx(ctx context.Context, event audit.Event) error {
	if event.Action == "auth.mfa_verify_failed" || event.Action == "auth.mfa_attempts_exhausted" {
		return errors.New("audit store unavailable")
	}
	return fakes.Audit{DB: a.db}.RecordTx(ctx, event)
}

type failingMFAFailureAuditTx struct{ db *fakes.MemDB }

func (t failingMFAFailureAuditTx) InTx(ctx context.Context, fn func(ctx context.Context, tx core.TxRepos) error) error {
	return fakes.Tx{DB: t.db}.InTx(ctx, func(ctx context.Context, tx core.TxRepos) error {
		tx.Audit = failingMFAFailureAudit(t)
		return fn(ctx, tx)
	})
}

// An audit write failure is logged, but the failed login still returns its
// 401: auditing must never turn an authentication failure into a 5xx.
func TestMFAFailureAuditErrorDoesNotChangeTheResponse(t *testing.T) {
	db := fakes.New()
	h := newMFAHarnessWithTx(t, db, failingMFAFailureAuditTx{db: db})
	ctx := fakes.TenantCtx()
	h.enrollAndConfirm(t)

	challenge, err := h.svc.BeginChallenge(ctx, fakes.TestID(100), core.AMRPassword)
	require.NoError(t, err)
	_, err = h.svc.Verify(ctx, challenge, "not-a-real-code")
	require.Error(t, err)
	assert.Equal(t, string(identitycatalog.CodeAuthMFAInvalidCode), errorCode(err))
	assert.Empty(t, failures(db.Audits, "auth.mfa_verify_failed"), "the failing writer records nothing")
}
