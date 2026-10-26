package service

import (
	"context"
	"errors"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// MFAStepUpService refreshes the AAL2 freshness of the caller's current
// session after a fresh second-factor proof. It is separate from MFAService so
// the management/login surface does not gain a session-mutating method it does
// not need.
type MFAStepUpService interface {
	// StepUp verifies a TOTP or recovery code, spends the same per-user
	// second-factor budget as the other verification paths, and updates
	// mfa_verified_at on the caller's own session. It returns the new
	// timestamp.
	StepUp(ctx context.Context, userID, sessionID int64, code string) (time.Time, error)
}

var _ MFAStepUpService = (*mfaService)(nil)

func (s *mfaService) StepUp(ctx context.Context, userID, sessionID int64, code string) (time.Time, error) {
	if sessionID <= 0 {
		return time.Time{}, apperror.NewUnauthorized("session is required")
	}
	matched, attempts, err := s.checkCode(ctx, userID, code)
	if err != nil {
		s.auditSecondFactorFailure(ctx, userID, attempts, err)
		return time.Time{}, err
	}

	var verifiedAt time.Time
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		// Claim the code first, exactly like login verification: a wrong or
		// replayed code must not refresh the session's AAL2 clock.
		if err := s.consumeCode(ctx, tx.Stores, userID, matched); err != nil {
			return err
		}
		ts, err := tx.Sessions.MarkMFAVerified(ctx, userID, sessionID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewUnauthorized("session is no longer valid")
			}
			return apperror.NewInternal("failed to refresh MFA verification")
		}
		verifiedAt = ts
		event := userEvent(ctx, "auth.mfa_step_up", userID, map[string]any{
			"method": matched.auditMethod(),
		})
		event.ActorID = userID
		event.ActorType = audit.ActorTypeUser
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	if err != nil {
		s.auditSecondFactorFailure(ctx, userID, 0, err)
		return time.Time{}, err
	}
	clearMFAAttempts(ctx, s.Cache, userID)
	// The session snapshot (and every cached permission snapshot keyed with
	// it) still carries the old timestamp; drop it so the next request sees
	// the refreshed AAL2 clock immediately.
	if s.Sessions != nil {
		s.Sessions.invalidate(ctx, userID)
	}
	return verifiedAt, nil
}
