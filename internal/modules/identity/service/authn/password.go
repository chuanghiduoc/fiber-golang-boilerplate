package authn

import (
	"context"
	"errors"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// ChangePasswordInput sets a new password after verifying the current one.
type ChangePasswordInput struct {
	CurrentPassword string
	NewPassword     string
}

func currentPasswordIncorrect() error {
	return apperror.NewBadRequest("current password is incorrect")
}

// VerifyPassword checks a plaintext password against the stored hash. It is
// used to require re-authentication for sensitive profile changes.
func (s *userService) VerifyPassword(ctx context.Context, userID uuid.UUID, password string) error {
	member, err := getMember(ctx, s.Stores.Memberships, userID)
	if err != nil {
		return err
	}
	hash, err := s.Stores.Users.PasswordHash(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return currentPasswordIncorrect()
		}
		return apperror.NewInternal("failed to get user")
	}

	// Shares the login counters (per source and per account) so
	// re-authentication cannot be used to guess the password in parallel.
	emailAddr := member.User.Email
	if err := reserveLoginAttempt(ctx, s.Cache, emailAddr); err != nil {
		return err
	}
	if ok, _ := verifyPassword(hash, password); !ok {
		return currentPasswordIncorrect()
	}
	clearLoginAttempts(ctx, s.Cache, emailAddr)
	return nil
}

func (s *userService) ChangePassword(ctx context.Context, userID uuid.UUID, req ChangePasswordInput) error {
	member, err := getMember(ctx, s.Stores.Memberships, userID)
	if err != nil {
		return err
	}
	if !member.User.HasPassword {
		return apperror.NewBadRequest("cannot change password for OAuth accounts")
	}
	if err := s.VerifyPassword(ctx, userID, req.CurrentPassword); err != nil {
		return err
	}
	if req.CurrentPassword == req.NewPassword {
		return apperror.NewBadRequest("new password must be different from the current password")
	}

	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		return err
	}

	// The password row and the token-version bump live in the request tenant's
	// transaction; they are global statements and need no RLS bypass. The bump
	// alone ends every session in every tenant, so the per-tenant session-row
	// revocation is a follow-up bypass statement after this commits.
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if changeErr := replacePassword(ctx, tx.Stores, userID, hash); changeErr != nil {
			return changeErr
		}
		if notifyErr := s.Security.Deliver(ctx, tx, SecurityNotice{
			Event:      SecurityEventPasswordChanged,
			UserID:     userID,
			Name:       member.User.Name,
			Email:      member.User.Email,
			OccurredAt: time.Now().UTC(),
		}); notifyErr != nil {
			return notifyErr
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "user.password_changed", userID, nil)))
	})
	if err != nil {
		return err
	}

	cleanupStaleSessions(ctx, s.Stores.Sessions, userID, RevokeCredentialsChanged)
	invalidateSessions(ctx, s.SessionCache, userID)
	clearLoginAttempts(ctx, s.Cache, member.User.Email)
	return nil
}

// replacePassword stores the new hash (which bumps the token version), removes a
// pending MFA enrollment, and invalidates reset links: a password change ends
// every existing session of the user in every tenant (through the version bump;
// the caller records the session-row cleanup after commit) and whoever started
// an unconfirmed enrollment no longer proves account control.
func replacePassword(ctx context.Context, st Stores, userID uuid.UUID, hash string) error {
	if _, err := st.Users.ReplacePassword(ctx, userID, hash); err != nil {
		return userWriteError(err, "user not found", "failed to update password")
	}
	if err := st.MFA.DeletePendingTOTP(ctx, userID); err != nil {
		return apperror.NewInternal("failed to delete pending MFA enrollment")
	}
	if err := st.Resets.DeleteByUser(ctx, userID); err != nil {
		return apperror.NewInternal("failed to delete reset tokens")
	}
	return nil
}
