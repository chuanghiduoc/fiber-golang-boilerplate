package mfa

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// reauthVerifier proves the caller may change the account's security settings.
// Password accounts verify the current password after spending a separate,
// fail-closed re-authentication budget. Passwordless accounts need an
// interactive session no older than core.ReauthSessionMaxAge, so a stolen access
// token alone cannot add/remove credentials or change MFA.
type reauthVerifier struct {
	stores   Stores
	attempts cache.Cache
	now      func() time.Time
}

func (v reauthVerifier) verify(ctx context.Context, userID uuid.UUID, proof ReauthProof) error {
	user, err := v.stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to load user")
	}
	if !user.HasPassword {
		return v.verifyRecentSession(ctx, userID, proof.SessionID)
	}
	// A missing proof is not a password guess: reject it before spending the
	// shared re-authentication budget, so spamming an empty body cannot lock
	// the account out. A supplied but wrong password still spends the budget.
	if proof.CurrentPassword == "" {
		return mfaReauthRequired()
	}
	if v.attempts == nil {
		return loginLockoutUnavailable(errors.New("MFA attempt store is not configured"))
	}
	if err := reserveReauthAttempt(ctx, v.attempts, userID); err != nil {
		return err
	}
	hash, err := v.stores.Users.PasswordHash(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return currentPasswordIncorrect()
		}
		return apperror.NewInternal("failed to get user")
	}
	if ok, _ := verifyPassword(hash, proof.CurrentPassword); !ok {
		return currentPasswordIncorrect()
	}
	clearReauthAttempts(ctx, v.attempts, userID)
	return nil
}

func (v reauthVerifier) verifyRecentSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	live, err := core.RecentSessionIsLive(ctx, v.stores, userID, sessionID, v.now())
	if err != nil {
		return apperror.NewInternal("failed to load session")
	}
	if !live {
		return mfaReauthRequired()
	}
	return nil
}
