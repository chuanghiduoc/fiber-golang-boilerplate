package service

import (
	"context"
	"errors"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// ValidatePasswordForEmail rejects a password that is or contains the local
// part of the address it belongs to (NIST SP 800-63B memorized-secret rule).
// The tag validator cannot see another field, so registration, password
// change, and password reset call this where the email is known.
func ValidatePasswordForEmail(password, email, field string) error {
	if !validator.PasswordContainsEmailLocalPart(password, email) {
		return nil
	}
	return apperror.NewValidation("One or more fields did not pass validation.", []apperror.FieldError{{
		Path:    field,
		Code:    validator.CodePasswordFromEmail,
		Message: field + " must not be or contain your email address",
	}})
}

// PasswordResetEmailLookup is an optional capability of
// PasswordResetService. The reset endpoint needs the address behind a live
// token only to apply the email/password policy before the password is
// stored; an unknown or expired token returns "" and the reset itself reports
// the authoritative error.
type PasswordResetEmailLookup interface {
	EmailForResetToken(ctx context.Context, token string) (string, error)
}

// EmailForResetToken resolves the address of a live reset token.
func (s *passwordResetService) EmailForResetToken(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	reset, err := s.Stores.Resets.GetByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return "", nil
		}
		return "", apperror.NewInternal("failed to verify reset token")
	}
	if !reset.ExpiresAt.After(time.Now()) {
		return "", nil
	}
	user, err := s.Stores.Users.GetByID(ctx, reset.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return "", nil
		}
		return "", apperror.NewInternal("failed to load user")
	}
	return user.Email, nil
}
