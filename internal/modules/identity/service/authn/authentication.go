package authn

import (
	"context"
	"errors"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// LoginInput is one password sign-in attempt in the request tenant.
type LoginInput struct {
	Email    string
	Password string
}

func invalidCredentials() error {
	return apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
}

// Authenticate verifies the global password, then requires an active
// membership in the request tenant. A correct password for an account that is
// not an active member of this tenant (never joined, banned, or removed) gets
// the same 401 as a wrong password, so the response does not tell an
// attacker holding a leaked password which tenants the account belongs to.
func (s *userService) Authenticate(ctx context.Context, req LoginInput) (*Member, error) {
	email := NormalizeEmail(req.Email)

	// Reserve an attempt before any lookup or password check. The counters
	// are keyed by the normalized address (plus the client IP for the source
	// budget) whether or not an account exists, so a locked response cannot
	// reveal existence, and the atomic increments mean parallel guesses cannot
	// all pass a stale "not locked yet" read. Registration clears the keys, so
	// pre-locking an unregistered address cannot lock out its future owner.
	if err := reserveLoginAttempt(ctx, s.Cache, email); err != nil {
		return nil, err
	}

	user, hash, err := s.loadCredential(ctx, email)
	if err != nil {
		return nil, err
	}
	if hash == "" {
		// Unknown or passwordless (OAuth-only) account: spend the same Argon2id
		// time as an existing account so timing cannot enumerate accounts.
		spendDummyPasswordWork(req.Password)
		return nil, invalidCredentials()
	}
	ok, needsRehash := verifyPassword(hash, req.Password)
	if !ok {
		return nil, invalidCredentials()
	}
	// A successful sign-in upgrades a hash written under older cost
	// parameters (OWASP Password Storage: verify-and-rehash).
	if needsRehash {
		upgraded, err := hashPassword(req.Password)
		if err != nil {
			return nil, err
		}
		if err := s.Stores.Users.RehashPassword(ctx, user.ID, hash, upgraded); err != nil {
			return nil, apperror.NewInternal("failed to upgrade password hash")
		}
	}

	// The password was correct: clear this source's counter even if the
	// account still needs verification, so a correct password never accrues a
	// lockout from one address. The account-wide counter stays: it records
	// distributed guessing, which one successful login must not erase.
	clearLoginSourceAttempts(ctx, s.Cache, email)

	if user.Status != UserStatusActive {
		return nil, invalidCredentials()
	}
	member, err := s.Stores.Memberships.GetActive(ctx, user.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, invalidCredentials()
		}
		return nil, apperror.NewInternal("failed to load membership")
	}
	if s.RequireEmailVerification && !member.User.EmailVerified() {
		return nil, apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return member, nil
}

// loadCredential returns the account and its password hash; the hash is ""
// for an unknown or passwordless account.
// for an unknown or passwordless account.
func (s *userService) loadCredential(ctx context.Context, email string) (account *User, passwordHash string, loadErr error) {
	user, err := s.Stores.Users.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, "", nil
		}
		return nil, "", apperror.NewInternal("failed to get user")
	}
	hash, err := s.Stores.Users.PasswordHash(ctx, user.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return user, "", nil
		}
		return nil, "", apperror.NewInternal("failed to get user")
	}
	return user, hash, nil
}
