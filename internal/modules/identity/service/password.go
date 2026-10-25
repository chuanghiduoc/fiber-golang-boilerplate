package service

import (
	"context"
	"errors"

	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
func (s *userService) VerifyPassword(ctx context.Context, userID int64, password string) error {
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
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return currentPasswordIncorrect()
	}
	clearLoginAttempts(ctx, s.Cache, emailAddr)
	return nil
}

func (s *userService) ChangePassword(ctx context.Context, userID int64, req ChangePasswordInput) error {
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

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if changeErr := replacePassword(ctx, tx.Stores, userID, string(hash)); changeErr != nil {
			return changeErr
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "user.password_changed", userID, nil)))
	})
	if err != nil {
		return err
	}

	invalidateSessions(ctx, s.SessionCache, userID)
	clearLoginAttempts(ctx, s.Cache, member.User.Email)
	return nil
}

// replacePassword stores the new hash (which bumps the token version) and
// revokes every session and reset link: a password change ends every
// existing session of the user in every tenant.
func replacePassword(ctx context.Context, st Stores, userID int64, hash string) error {
	if _, err := st.Users.ReplacePassword(ctx, userID, hash); err != nil {
		return userWriteError(err, "user not found", "failed to update password")
	}
	if err := st.Sessions.RevokeAllForUser(ctx, userID, RevokeCredentialsChanged); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	if err := st.Resets.DeleteByUser(ctx, userID); err != nil {
		return apperror.NewInternal("failed to delete reset tokens")
	}
	return nil
}
