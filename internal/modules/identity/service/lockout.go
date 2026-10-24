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
	// maxLoginAttempts is the per-(tenant, email, client IP) budget. Keying by
	// source means a stranger cannot lock a victim out from one address.
	maxLoginAttempts = 5
	// accountLockoutMultiplier sets the global per-(tenant, email) budget,
	// which still stops guessing distributed across many addresses. One source
	// contributes at most maxLoginAttempts per window to it.
	accountLockoutMultiplier = 5
	maxAccountLoginAttempts  = accountLockoutMultiplier * maxLoginAttempts
	lockoutDuration          = 15 * time.Minute
	loginAttemptPrefix       = "login_attempts:"
)

// loginAttemptKeys returns the per-source key (tenant, email, client IP) and
// the global per-account key (tenant, email) for one password attempt. The
// email is hashed by scopedKey, so neither key contains the address.
func loginAttemptKeys(ctx context.Context, email string) (source, account string) {
	account = scopedKey(ctx, loginAttemptPrefix, email)
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
