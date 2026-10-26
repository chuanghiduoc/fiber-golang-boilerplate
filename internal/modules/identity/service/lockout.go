package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

const (
	// maxLoginAttempts is the per-(email, client IP) budget. Keying by source
	// means a stranger cannot lock a victim out from one address.
	maxLoginAttempts = 5
	// accountLockoutMultiplier sets the global per-email budget, which still
	// stops guessing distributed across many addresses. One source
	// contributes at most maxLoginAttempts per window to it.
	accountLockoutMultiplier = 5
	maxAccountLoginAttempts  = accountLockoutMultiplier * maxLoginAttempts
	lockoutDuration          = 15 * time.Minute
	loginAttemptPrefix       = "login_attempts:"

	// maxMFAAttempts bounds second-factor guesses per user across every
	// source. It is separate from the password counters because a correct
	// password clears those: sharing them would let a caller who knows the
	// password reset the TOTP guessing budget at will.
	maxMFAAttempts   = 5
	mfaAttemptPrefix = "mfa_attempts:"
)

// reserveMFAAttempt counts one second-factor attempt for userID and rejects
// it once the budget is spent. Only a successful verification clears it.
func reserveMFAAttempt(ctx context.Context, store cache.Cache, userID int64) error {
	attempts, err := store.Increment(ctx, mfaAttemptKey(userID), lockoutDuration)
	if err != nil {
		return loginLockoutUnavailable(err)
	}
	if attempts > maxMFAAttempts {
		return apperror.NewTooManyRequests(fmt.Sprintf("too many MFA attempts, try again in %d minutes", int(lockoutDuration.Minutes())))
	}
	return nil
}

func clearMFAAttempts(ctx context.Context, store cache.Cache, userID int64) {
	if err := store.Delete(ctx, mfaAttemptKey(userID)); err != nil {
		slog.Warn("cache error clearing MFA attempts", slog.Any("error", err))
	}
}

func mfaAttemptKey(userID int64) string {
	return fmt.Sprintf("%s%d", mfaAttemptPrefix, userID)
}

// loginAttemptKeys returns the per-source key (email, client IP) and the
// global per-account key (email) for one password attempt. The email is
// hashed by emailKey, so neither key contains the address. The password is
// a global credential, so neither key is tenant-scoped.
func loginAttemptKeys(ctx context.Context, email string) (source, account string) {
	account = emailKey(loginAttemptPrefix, email)
	return account + ":ip=" + clientIPFromContext(ctx), account
}

// reserveLoginAttempt atomically counts one password attempt and rejects it
// once the source made more than maxLoginAttempts, or all sources together
// more than maxAccountLoginAttempts, inside the lockout window. A source that
// is already locked is rejected before it touches the account counter, so one
// address alone can never exhaust the global budget. It fails closed: when
// the cache is unavailable the attempt is denied (503) rather than allowed
// without brute-force protection.
func reserveLoginAttempt(ctx context.Context, store cache.Cache, email string) error {
	sourceKey, accountKey := loginAttemptKeys(ctx, email)
	locked := apperror.NewTooManyRequests(fmt.Sprintf("account temporarily locked, try again in %d minutes", int(lockoutDuration.Minutes())))

	attempts, err := store.Increment(ctx, sourceKey, lockoutDuration)
	if err != nil {
		return loginLockoutUnavailable(err)
	}
	if attempts > maxLoginAttempts {
		return locked
	}
	attempts, err = store.Increment(ctx, accountKey, lockoutDuration)
	if err != nil {
		return loginLockoutUnavailable(err)
	}
	if attempts > maxAccountLoginAttempts {
		return locked
	}
	return nil
}

func loginLockoutUnavailable(err error) error {
	slog.Error("cache unavailable for login lockout check — denying request for safety", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemUnavailable, "unable to verify login attempts, please try again later")
}

// clearLoginAttempts drops the caller's source counter and the account
// counter after a proof of the password (or a registration/reset). Other
// sources' counters expire on their own.
func clearLoginAttempts(ctx context.Context, store cache.Cache, email string) {
	if store == nil {
		return
	}
	sourceKey, accountKey := loginAttemptKeys(ctx, email)
	for _, key := range []string{sourceKey, accountKey} {
		if err := store.Delete(ctx, key); err != nil {
			slog.Warn("cache error clearing login attempts", slog.Any("error", err))
		}
	}
}
