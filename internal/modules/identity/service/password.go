package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// VerifyPassword checks a plaintext password against the stored hash. It is
// used to require re-authentication for sensitive profile changes.
func (s *userService) VerifyPassword(ctx context.Context, userID int64, password string) error {
	user, err := s.getUser(ctx, userID)
	if err != nil {
		return err
	}
	if !user.PasswordHash.Valid {
		return apperror.NewBadRequest("current password is incorrect")
	}

	// Shares the login counters (per source and per account) so
	// re-authentication cannot be used to guess the password in parallel.
	emailAddr := NormalizeEmail(user.Email)
	if err := reserveLoginAttempt(ctx, s.cache, emailAddr); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(password)) != nil {
		return apperror.NewBadRequest("current password is incorrect")
	}
	clearLoginAttempts(ctx, s.cache, emailAddr)
	return nil
}

func (s *userService) ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error {
	user, err := s.getUser(ctx, userID)
	if err != nil {
		return err
	}
	if !user.PasswordHash.Valid {
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

	err = inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		if changeErr := replacePassword(ctx, r, userID, hash); changeErr != nil {
			return changeErr
		}
		return checkAudit(audit.RecordInTx(ctx, s.audit, tx, userEvent(ctx, "user.password_changed", userID, nil)))
	})
	if err != nil {
		return err
	}

	invalidateSessions(ctx, s.sessions, userID)
	clearLoginAttempts(ctx, s.cache, NormalizeEmail(user.Email))
	return nil
}

// replacePassword stores the new hash (which bumps the token version) and
// revokes every refresh token: a password change ends every existing session.
func replacePassword(ctx context.Context, r repos, userID int64, hash []byte) error {
	if _, err := r.users.UpdatePassword(ctx, identitysqlc.UpdateUserPasswordParams{
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		ID:           userID,
	}); err != nil {
		return apperror.NewInternal("failed to update password")
	}
	if err := r.refresh.DeleteByUserID(ctx, userID); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	return nil
}
