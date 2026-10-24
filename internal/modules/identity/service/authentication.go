package service

import (
	"context"
	"errors"

	"golang.org/x/crypto/bcrypt"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// dummyPasswordHash equalizes the bcrypt work for unknown-email logins so the
// response time does not reveal whether an account exists.
var dummyPasswordHash = func() []byte {
	hash, _ := bcrypt.GenerateFromPassword([]byte("timing-only-dummy-password"), bcryptCost)
	return hash
}()

func invalidCredentials() error {
	return apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
}

func (s *userService) Authenticate(ctx context.Context, req dto.LoginRequest) (*dto.UserResponse, error) {
	email := NormalizeEmail(req.Email)

	// Reserve an attempt before any lookup or password check. The counters
	// are keyed by the normalized address (plus the client IP for the source
	// budget) whether or not an account exists, so a locked response cannot
	// reveal existence, and the atomic increments mean parallel guesses cannot
	// all pass a stale "not locked yet" read. Registration clears the keys, so
	// pre-locking an unregistered address cannot lock out its future owner.
	if err := reserveLoginAttempt(ctx, s.cache, email); err != nil {
		return nil, err
	}

	user, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			// Spend the same bcrypt time as an existing account so login
			// timing cannot be used for account enumeration.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
			return nil, invalidCredentials()
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	if !user.PasswordHash.Valid {
		// OAuth-only account: equalize timing with a password check.
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
		return nil, invalidCredentials()
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(req.Password)); err != nil {
		return nil, invalidCredentials()
	}

	// The password was correct: clear the counters even if the account still
	// needs verification, so a correct password never accrues a lockout.
	clearLoginAttempts(ctx, s.cache, email)

	if s.requireEmailVerification && !user.EmailVerifiedAt.Valid {
		return nil, apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return ToUserResponse(user), nil
}
