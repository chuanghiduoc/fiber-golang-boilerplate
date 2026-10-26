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
	// maxAccountLoginFailures is the account-wide budget across every source.
	// NIST SP 800-63B rev4 §5.2.2 caps consecutive authentication failures at
	// 100; a lower multiplier (the old 5x5=25) let five addresses lock any
	// account for the whole window. One source contributes at most
	// maxLoginAttempts before it is rejected without touching this counter, so
	// exhausting it takes at least 100 failures from 20 distinct addresses
	// inside one lockoutDuration. The budget is per lockout window rather than
	// strictly consecutive: a correct password from a locked source cannot be
	// checked, and it clears only its own source counter (not this one), so
	// distributed-guessing evidence survives until it expires.
	maxAccountLoginFailures = 100
	lockoutDuration         = 15 * time.Minute
	loginAttemptPrefix      = "login_attempts:"

	// maxMFAAttempts bounds second-factor guesses per user across every
	// source. It is separate from the password counters because a correct
	// password clears those: sharing them would let a caller who knows the
	// password reset the TOTP guessing budget at will.
	maxMFAAttempts   = 5
	mfaAttemptPrefix = "mfa_attempts:"

	// mfaReauthAttemptPrefix bounds password guesses made to re-authenticate
	// for MFA enrollment management. It is a separate per-user budget so a
	// successful re-authentication can clear the password failures without
	// touching the TOTP/ recovery-code budget.
	mfaReauthAttemptPrefix = "mfa_reauth_attempts:"
)

func mfaReauthAttemptKey(userID int64) string {
	return fmt.Sprintf("%s%d", mfaReauthAttemptPrefix, userID)
}

// reserveReauthAttempt atomically spends one password guess before the bcrypt
// comparison, so parallel requests cannot all pass a read-only budget check.
func reserveReauthAttempt(ctx context.Context, store cache.Cache, userID int64) error {
	attempts, err := store.Increment(ctx, mfaReauthAttemptKey(userID), lockoutDuration)
	if err != nil {
		return loginLockoutUnavailable(err)
	}
	if attempts > maxMFAAttempts {
		return apperror.NewTooManyRequests(fmt.Sprintf("too many MFA attempts, try again in %d minutes", int(lockoutDuration.Minutes())))
	}
	return nil
}

func clearReauthAttempts(ctx context.Context, store cache.Cache, userID int64) {
	if err := store.Delete(ctx, mfaReauthAttemptKey(userID)); err != nil {
		slog.Warn("cache error clearing MFA re-authentication attempts", slog.Any("error", err))
	}
}

// reserveMFAAttempt counts one second-factor attempt for userID and rejects
// it once the budget is spent. It returns the new count so the caller can
// audit the exhaustion once (on the attempt that crosses the limit). Only a
// successful verification clears it.
func reserveMFAAttempt(ctx context.Context, store cache.Cache, userID int64) (int64, error) {
	attempts, err := store.Increment(ctx, mfaAttemptKey(userID), lockoutDuration)
	if err != nil {
		return 0, loginLockoutUnavailable(err)
	}
	if attempts > maxMFAAttempts {
		return attempts, apperror.NewTooManyRequests(fmt.Sprintf("too many MFA attempts, try again in %d minutes", int(lockoutDuration.Minutes())))
	}
	return attempts, nil
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
// more than maxAccountLoginFailures, inside the lockout window. A source that
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
	if attempts > maxAccountLoginFailures {
		return locked
	}
	return nil
}

func loginLockoutUnavailable(err error) error {
	slog.Error("cache unavailable for login lockout check — denying request for safety", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemUnavailable, "unable to verify login attempts, please try again later")
}

// clearLoginAttempts drops the caller's source counter and the account
// counter. Registration, a completed password change/reset, and a successful
// second factor call it: those prove account control, so every preexisting
// lockout may go.
func clearLoginAttempts(ctx context.Context, store cache.Cache, email string) {
	clearLoginCounter(ctx, store, email, true)
}

// clearLoginSourceAttempts drops only the caller's (email, client IP) counter
// after a correct password. The account-wide counter is deliberately kept: it
// records failures from other sources, and a legitimate login must not erase
// the evidence of distributed guessing. It expires with the lockout window.
func clearLoginSourceAttempts(ctx context.Context, store cache.Cache, email string) {
	clearLoginCounter(ctx, store, email, false)
}

func clearLoginCounter(ctx context.Context, store cache.Cache, email string, includeAccount bool) {
	if store == nil {
		return
	}
	sourceKey, accountKey := loginAttemptKeys(ctx, email)
	keys := []string{sourceKey}
	if includeAccount {
		keys = append(keys, accountKey)
	}
	for _, key := range keys {
		if err := store.Delete(ctx, key); err != nil {
			slog.Warn("cache error clearing login attempts", slog.Any("error", err))
		}
	}
}
