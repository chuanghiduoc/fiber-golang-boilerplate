package mfa

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
	"github.com/google/uuid"
)

const (
	mfaChallengeTTL      = 5 * time.Minute
	mfaChallengeVersion  = 1
	mfaRecoveryCodeCount = 10
	// mfaRecoveryCodeBytes is 15 random bytes (120 bits), above the NIST
	// SP 800-63B lookup-secret minimum of 112 bits, so a fast SHA-256 digest
	// at rest still leaves a brute-force margin.
	mfaRecoveryCodeBytes = 15
	mfaChallengeBytes    = 16
	mfaDefaultIssuer     = "Fiber Golang Boilerplate"
	// mfaEnrollmentTTL bounds a pending enrollment: its secret is generated
	// once (re-enrolling replaces it) and can only be confirmed while fresh.
	mfaEnrollmentTTL = 15 * time.Minute
	// mfaReauthSessionMaxAge is how recent a passwordless (Google-only)
	// user's interactive sign-in must be to enroll or confirm a factor. The
	// session row's created_at records that sign-in.
	mfaReauthSessionMaxAge = 15 * time.Minute
)

var recoveryEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// MFAService manages TOTP factors, recovery codes, and the two-step login
// challenge.
type MFAService interface {
	// EnrollTOTP starts (or restarts) a pending TOTP enrollment and returns
	// the provisioning URI; the secret is never returned again. The caller
	// re-authenticates with the current password, or (passwordless accounts)
	// with a session created within the last 15 minutes.
	EnrollTOTP(ctx context.Context, userID, sessionID uuid.UUID, currentPassword string) (*MFAEnrollment, error)
	// ConfirmTOTP verifies the first code, confirms the factor, replaces the
	// recovery codes, and returns them once. Like enrollment it requires
	// re-authentication. It bumps the user's token version (every session that
	// predates the factor is invalid) and re-issues the session that confirmed
	// it under the new version.
	ConfirmTOTP(ctx context.Context, userID, sessionID uuid.UUID, currentPassword, code string) (*MFAConfirmResult, error)
	// Disable removes the factor and recovery codes after a TOTP or recovery
	// code proof.
	Disable(ctx context.Context, userID, sessionID uuid.UUID, code string) error
	// RegenerateRecoveryCodes replaces the set after a code proof and returns
	// the new codes once.
	RegenerateRecoveryCodes(ctx context.Context, userID uuid.UUID, code string) ([]string, error)
	// ResetTOTP is the platform recovery path: it removes the confirmed and
	// pending factor plus recovery codes, revokes every session, and bumps the
	// token version. It does not require the user's password or a code.
	ResetTOTP(ctx context.Context, userID uuid.UUID) error
	Status(ctx context.Context, userID uuid.UUID) (*MFAStatus, error)
	// Required reports whether the user has a confirmed factor.
	Required(ctx context.Context, userID uuid.UUID) (bool, error)
	// BeginChallenge seals a short-lived, single-use, tenant-bound challenge
	// after the first factor (method pwd or google) succeeded.
	BeginChallenge(ctx context.Context, userID uuid.UUID, method string) (string, error)
	// Verify consumes a challenge and a TOTP or recovery code and issues the
	// normal token pair.
	Verify(ctx context.Context, challenge, code string) (*MFALogin, error)
}

// MFADeps are the MFA service's collaborators.
type MFADeps struct {
	Stores   Stores
	Tx       Transactor
	Cache    cache.Cache
	Sealer   SecretSealer
	Sessions *SessionService
	Issuer   string
	// Now is a test seam; nil uses time.Now.
	Now func() time.Time
	// Security sends the MFA change notifications. Nil disables them.
	Security *SecurityNotifier
}

type mfaService struct {
	MFADeps
}

// NewMFAService wires the MFA service.
func NewMFAService(deps MFADeps) MFAService {
	if deps.Issuer == "" {
		deps.Issuer = mfaDefaultIssuer
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &mfaService{MFADeps: deps}
}

func (s *mfaService) now() time.Time { return s.Now() }

func mfaInvalidCode() error {
	return apperror.NewCode(identitycatalog.CodeAuthMFAInvalidCode, "invalid MFA code")
}

func mfaChallengeInvalid() error {
	return apperror.NewCode(identitycatalog.CodeAuthMFAChallengeInvalid, "invalid or expired MFA challenge")
}

func mfaNotEnabled() error {
	return apperror.NewCode(identitycatalog.CodeAuthMFANotEnabled, "MFA is not enabled")
}

func mfaAlreadyEnabled() error {
	return apperror.NewCode(identitycatalog.CodeAuthMFAAlreadyEnabled, "MFA is already enabled")
}

func mfaEnrollmentExpired() error {
	return apperror.NewCode(identitycatalog.CodeAuthMFAEnrollmentExpired, "MFA enrollment has expired")
}

func mfaReauthRequired() error {
	return apperror.NewCode(identitycatalog.CodeAuthMFAReauthRequired, "re-authentication is required")
}

func (s *mfaService) Required(ctx context.Context, userID uuid.UUID) (bool, error) {
	_, err := s.Stores.MFA.GetConfirmedTOTP(ctx, userID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return false, nil
	}
	return false, apperror.NewInternal("failed to load MFA factor")
}

func (s *mfaService) Status(ctx context.Context, userID uuid.UUID) (*MFAStatus, error) {
	factor, err := s.Stores.MFA.GetConfirmedTOTP(ctx, userID)
	if errors.Is(err, apperror.ErrNotFound) {
		return &MFAStatus{}, nil
	}
	if err != nil {
		return nil, apperror.NewInternal("failed to load MFA factor")
	}
	remaining, err := s.Stores.MFA.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return nil, apperror.NewInternal("failed to count recovery codes")
	}
	return &MFAStatus{
		Enabled:                true,
		ConfirmedAt:            factor.ConfirmedAt,
		RecoveryCodesRemaining: remaining,
	}, nil
}

func (s *mfaService) EnrollTOTP(ctx context.Context, userID, sessionID uuid.UUID, currentPassword string) (*MFAEnrollment, error) {
	if err := s.reauthenticate(ctx, userID, sessionID, currentPassword); err != nil {
		return nil, err
	}
	if _, err := s.Stores.MFA.GetConfirmedTOTP(ctx, userID); err == nil {
		return nil, mfaAlreadyEnabled()
	} else if !errors.Is(err, apperror.ErrNotFound) {
		return nil, apperror.NewInternal("failed to load MFA factor")
	}

	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate MFA secret")
	}
	sealed, err := s.Sealer.Seal([]byte(secret))
	if err != nil {
		return nil, apperror.NewInternal("failed to seal MFA secret")
	}

	var factorID uuid.UUID
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		factor, err := tx.MFA.UpsertUnconfirmedTOTP(ctx, userID, string(sealed))
		if err != nil {
			return apperror.Internal(err, "failed to store MFA factor")
		}
		factorID = factor.ID
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "auth.mfa_enrollment_started", userID, map[string]any{
			"type": MFAFactorTOTP,
		})))
	})
	if err != nil {
		return nil, err
	}
	// Re-enrollment always starts over with a fresh secret (last writer wins).
	// Only the most recently stored pending secret can be confirmed, so this
	// caller's URI is the one that works unless a concurrent enroll replaces it.
	return &MFAEnrollment{FactorID: factorID, OtpauthURI: totp.URI(s.Issuer, user.Email, secret)}, nil
}

func (s *mfaService) ConfirmTOTP(ctx context.Context, userID, sessionID uuid.UUID, currentPassword, code string) (*MFAConfirmResult, error) {
	if err := s.reauthenticate(ctx, userID, sessionID, currentPassword); err != nil {
		return nil, err
	}
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate recovery codes")
	}
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	result := &MFAConfirmResult{RecoveryCodes: plain}
	// The factor, recovery codes, and token version are global rows, so this is
	// a normal tenant-scoped transaction. Bumping users.token_version
	// invalidates every session of the user in every tenant at once — including
	// a password-only login that read "MFA not required" just before this
	// commits, which is the login race the factor guard in CreateSession also
	// closes. Confirmation revokes only the user's OTHER sessions, so the
	// session that confirmed the factor is re-issued under the new version and
	// keeps working. The per-tenant session-row revocation is recorded by the
	// follow-up bypass statement after this commits.
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := s.confirmFactor(ctx, tx, userID, code); err != nil {
			return err
		}
		if err := tx.MFA.DeleteRecoveryCodes(ctx, userID); err != nil {
			return apperror.NewInternal("failed to replace recovery codes")
		}
		if err := tx.MFA.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
			return apperror.NewInternal("failed to store recovery codes")
		}
		if _, err := tx.Users.RevokeTokens(ctx, userID); err != nil {
			return apperror.NewInternal("failed to bump token version")
		}
		if s.Sessions != nil && sessionID != uuid.Nil {
			tokens, err := s.Sessions.ReissueTx(ctx, tx.Stores, userID, sessionID, AMROTP)
			if err != nil {
				return err
			}
			result.Tokens = tokens
		}
		return s.recordMFAChange(ctx, tx, SecurityEventMFAEnabled, user, "auth.mfa_enabled", map[string]any{
			"type":           MFAFactorTOTP,
			"recovery_codes": len(plain),
		})
	})
	if err != nil {
		return nil, err
	}
	cleanupStaleSessions(ctx, s.Stores.Sessions, userID, RevokeMFAChanged)
	if s.Sessions != nil {
		s.Sessions.Invalidate(ctx, userID)
	}
	return result, nil
}

// confirmFactor locks the pending factor, verifies the first code, and marks
// the factor confirmed. The row lock keeps a concurrent enroll from replacing
// the secret between verification and the confirm update.
func (s *mfaService) confirmFactor(ctx context.Context, tx TxRepos, userID uuid.UUID, code string) error {
	factor, err := tx.MFA.GetTOTPForUpdate(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return mfaNotEnabled()
		}
		return apperror.NewInternal("failed to load MFA factor")
	}
	if factor.Confirmed() {
		return mfaAlreadyEnabled()
	}
	if s.now().After(factor.UpdatedAt.Add(mfaEnrollmentTTL)) {
		return mfaEnrollmentExpired()
	}
	secret, err := s.Sealer.Open([]byte(factor.SecretCiphertext))
	if err != nil {
		return apperror.NewInternal("failed to open MFA secret")
	}
	step, ok := totp.Validate(string(secret), code, s.now())
	if !ok {
		return mfaInvalidCode()
	}
	confirmed, err := tx.MFA.ConfirmTOTP(ctx, factor.ID, userID, step)
	if err != nil {
		return apperror.NewInternal("failed to confirm MFA factor")
	}
	if !confirmed {
		return mfaAlreadyEnabled()
	}
	return nil
}

// reauthenticate proves the caller may change the account's second factor.
// A local password is checked against the Argon2id hash; every attempt spends
// the per-user re-authentication budget before the comparison, so parallel
// guesses cannot bypass it. A correct password clears that password budget
// (but never the TOTP/recovery-code budget). A passwordless account instead
// needs an interactive sign-in no older than mfaReauthSessionMaxAge: a stolen
// access token alone must not bind a new authenticator.
func (s *mfaService) reauthenticate(ctx context.Context, userID, sessionID uuid.UUID, currentPassword string) error {
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to load user")
	}
	if !user.HasPassword {
		return s.reauthenticateSession(ctx, userID, sessionID)
	}
	if s.Cache == nil {
		return loginLockoutUnavailable(errors.New("MFA attempt store is not configured"))
	}
	// Reserve atomically before the Argon2id comparison so parallel guesses
	// cannot each pass a read-only budget check. This budget is separate from
	// the TOTP/recovery-code budget; a success clears it and leaves the TOTP
	// budget untouched.
	if err := reserveReauthAttempt(ctx, s.Cache, userID); err != nil {
		return err
	}
	hash, err := s.Stores.Users.PasswordHash(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return currentPasswordIncorrect()
		}
		return apperror.NewInternal("failed to get user")
	}
	if ok, _ := verifyPassword(hash, currentPassword); !ok {
		return currentPasswordIncorrect()
	}
	clearReauthAttempts(ctx, s.Cache, userID)
	return nil
}

func (s *mfaService) reauthenticateSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	sess, err := s.Stores.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return mfaReauthRequired()
		}
		return apperror.NewInternal("failed to load session")
	}
	if sess.UserID != userID || sess.TenantID != tenant.IDFromContext(ctx) ||
		sess.RevokedAt != nil || !sess.ExpiresAt.After(s.now()) ||
		s.now().Sub(sess.CreatedAt) > mfaReauthSessionMaxAge {
		return mfaReauthRequired()
	}
	return nil
}

// ResetTOTP removes every second factor of a global user and ends all their
// sessions. The platform route calls it when a user lost both the
// authenticator and the recovery codes.
func (s *mfaService) ResetTOTP(ctx context.Context, userID uuid.UUID) error {
	// The factor, recovery codes, and token version are global rows, so this is
	// a normal tenant-scoped transaction; the version bump ends the target
	// user's sessions in every tenant at once. The per-tenant session-row
	// revocation is recorded by the follow-up bypass statement after commit.
	// The audit event names the administrator's request tenant explicitly.
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		user, err := tx.Users.GetByID(ctx, userID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to load user")
		}
		if err := tx.MFA.DeleteTOTP(ctx, userID); err != nil {
			return apperror.NewInternal("failed to delete MFA factor")
		}
		if err := tx.MFA.DeleteRecoveryCodes(ctx, userID); err != nil {
			return apperror.NewInternal("failed to delete recovery codes")
		}
		if _, err := tx.Users.RevokeTokens(ctx, userID); err != nil {
			return apperror.NewInternal("failed to bump token version")
		}
		return s.recordMFAChange(ctx, tx, SecurityEventMFAReset, user, "admin.user.mfa_reset", map[string]any{
			"scope": "platform",
		})
	})
	if err != nil {
		return err
	}
	cleanupStaleSessions(ctx, s.Stores.Sessions, userID, RevokeMFAChanged)
	if s.Sessions != nil {
		s.Sessions.Invalidate(ctx, userID)
	}
	return nil
}

func (s *mfaService) Disable(ctx context.Context, userID, sessionID uuid.UUID, code string) error {
	matched, attempts, err := s.checkCode(ctx, userID, code)
	if err != nil {
		s.auditSecondFactorFailure(ctx, userID, attempts, err)
		return err
	}
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to load user")
	}
	// Disabling the factor and ending the user's other sessions in every tenant
	// must not half-apply with the factor deletion. The factor rows are global,
	// so the transaction is tenant-scoped; disabling a factor does not
	// invalidate an older session (there is no second factor left to bypass),
	// so the caller's own session stays live and the cross-tenant revocation of
	// the others is the follow-up bypass statement after commit. The audit
	// event names the request tenant explicitly.
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := s.consumeCode(ctx, tx.Stores, userID, matched); err != nil {
			return err
		}
		if err := tx.MFA.DeleteTOTP(ctx, userID); err != nil {
			return apperror.NewInternal("failed to delete MFA factor")
		}
		if err := tx.MFA.DeleteRecoveryCodes(ctx, userID); err != nil {
			return apperror.NewInternal("failed to delete recovery codes")
		}
		return s.recordMFAChange(ctx, tx, SecurityEventMFADisabled, user, "auth.mfa_disabled", map[string]any{
			"method": matched.auditMethod(),
		})
	})
	if err != nil {
		s.auditSecondFactorFailure(ctx, userID, 0, err)
		return err
	}
	cleanupOtherSessions(ctx, s.Stores.Sessions, userID, sessionID, RevokeMFAChanged)
	clearMFAAttempts(ctx, s.Cache, userID)
	if s.Sessions != nil {
		s.Sessions.Invalidate(ctx, userID)
	}
	return nil
}

func (s *mfaService) RegenerateRecoveryCodes(ctx context.Context, userID uuid.UUID, code string) ([]string, error) {
	matched, attempts, err := s.checkCode(ctx, userID, code)
	if err != nil {
		s.auditSecondFactorFailure(ctx, userID, attempts, err)
		return nil, err
	}
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate recovery codes")
	}
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := s.consumeCode(ctx, tx.Stores, userID, matched); err != nil {
			return err
		}
		if err := tx.MFA.DeleteRecoveryCodes(ctx, userID); err != nil {
			return apperror.NewInternal("failed to replace recovery codes")
		}
		if err := tx.MFA.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
			return apperror.NewInternal("failed to store recovery codes")
		}
		return s.recordMFAChange(ctx, tx, SecurityEventRecoveryCodesRegenerated, user, "auth.mfa_recovery_codes_regenerated", map[string]any{
			"method": matched.auditMethod(),
			"count":  len(plain),
		})
	})
	if err != nil {
		s.auditSecondFactorFailure(ctx, userID, 0, err)
		return nil, err
	}
	clearMFAAttempts(ctx, s.Cache, userID)
	return plain, nil
}

// recordMFAChange enqueues the account-owner notification and writes the Tier
// 1 audit row in the same transaction as the factor change, so the change, its
// notification job, and its audit row commit or roll back together.
func (s *mfaService) recordMFAChange(ctx context.Context, tx TxRepos, event SecurityEvent, user *User, action string, metadata map[string]any) error {
	if err := s.Security.Deliver(ctx, tx, SecurityNotice{
		Event:      event,
		UserID:     user.ID,
		Name:       user.Name,
		Email:      user.Email,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, action, user.ID, metadata)))
}

// mfaCode is a verified TOTP step or recovery-code hash that still has to be
// claimed atomically.
type mfaCode struct {
	method string
	step   int64
	hash   []byte
}

func (c *mfaCode) auditMethod() string { return c.method }

// checkCode counts one second-factor guess against the per-user MFA budget,
// then matches code. Callers clear the budget after the code is claimed and
// audit a returned error through auditSecondFactorFailure.
func (s *mfaService) checkCode(ctx context.Context, userID uuid.UUID, code string) (*mfaCode, int64, error) {
	if s.Cache == nil {
		return nil, 0, loginLockoutUnavailable(errors.New("MFA attempt store is not configured"))
	}
	attempts, err := reserveMFAAttempt(ctx, s.Cache, userID)
	if err != nil {
		return nil, attempts, err
	}
	matched, err := s.matchCode(ctx, s.Stores, userID, code)
	if err != nil {
		return nil, attempts, err
	}
	return matched, attempts, nil
}

// matchCode checks code against the confirmed factor. A non-matching TOTP
// value is treated as a recovery code; claimCode decides whether it exists.
func (s *mfaService) matchCode(ctx context.Context, st Stores, userID uuid.UUID, code string) (*mfaCode, error) {
	factor, err := st.MFA.GetConfirmedTOTP(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, mfaNotEnabled()
		}
		return nil, apperror.NewInternal("failed to load MFA factor")
	}
	secret, err := s.Sealer.Open([]byte(factor.SecretCiphertext))
	if err != nil {
		return nil, apperror.NewInternal("failed to open MFA secret")
	}
	if step, ok := totp.Validate(string(secret), code, s.now()); ok {
		return &mfaCode{method: MFAFactorTOTP, step: step}, nil
	}
	normalized := normalizeRecoveryCode(code)
	if normalized == "" {
		return nil, mfaInvalidCode()
	}
	return &mfaCode{method: "recovery_code", hash: hashToken(normalized)}, nil
}

// consumeCode claims the verified code. Only one caller can claim a step or a
// recovery code, so concurrent verifications cannot both succeed.
func (s *mfaService) consumeCode(ctx context.Context, st Stores, userID uuid.UUID, matched *mfaCode) error {
	var (
		claimed bool
		err     error
	)
	switch matched.method {
	case "recovery_code":
		claimed, err = st.MFA.UseRecoveryCode(ctx, userID, matched.hash)
	default:
		claimed, err = st.MFA.ConsumeTOTPStep(ctx, userID, matched.step)
	}
	if err != nil {
		return apperror.NewInternal("failed to verify MFA code")
	}
	if !claimed {
		return mfaInvalidCode()
	}
	return nil
}

// auditSecondFactorFailure records a failed TOTP/recovery-code attempt in its
// own transaction, so it persists although the request that failed rolls back
// or returns a 401/429. attempts is the budget counter after this guess: the
// 6th attempt (max+1) is the one that exhausted the budget, and only that one
// records the lockout event. An audit write failure is logged and swallowed:
// it must never turn a 401 into a 5xx.
func (s *mfaService) auditSecondFactorFailure(ctx context.Context, userID uuid.UUID, attempts int64, err error) {
	var appErr *apperror.AppError
	if userID == uuid.Nil || !errors.As(err, &appErr) {
		return
	}
	reason := ""
	switch {
	case appErr.Code == string(apperror.CodeRateLimitExceeded):
		reason = "budget_exhausted"
	case appErr.Code == string(identitycatalog.CodeAuthMFAInvalidCode):
		reason = "invalid_code"
	case appErr.Code == string(identitycatalog.CodeAuthMFANotEnabled):
		reason = "not_enabled"
	default:
		// Internal and conflict failures are not second-factor guesses.
		return
	}
	s.recordMFAFailure(ctx, userID, reason)
	if reason == "budget_exhausted" && attempts == MaxMFAAttempts+1 {
		s.recordMFABudgetExhausted(ctx, userID)
	}
}

func (s *mfaService) recordMFAFailure(ctx context.Context, userID uuid.UUID, reason string) {
	event := userEvent(ctx, "auth.mfa_verify_failed", userID, map[string]any{
		"reason": reason,
	})
	event.ActorID = userID
	event.ActorType = audit.ActorTypeUser
	event.Outcome = audit.OutcomeFailed
	s.recordMFAEventDetached(ctx, event)
}

func (s *mfaService) recordMFABudgetExhausted(ctx context.Context, userID uuid.UUID) {
	event := userEvent(ctx, "auth.mfa_attempts_exhausted", userID, map[string]any{
		"reason": "budget_exhausted",
	})
	event.ActorID = userID
	event.ActorType = audit.ActorTypeUser
	event.Outcome = audit.OutcomeFailed
	s.recordMFAEventDetached(ctx, event)
}

// recordMFAEventDetached writes a failure event in its own transaction. The
// request context may be canceled while it fails; the audit must still commit.
func (s *mfaService) recordMFAEventDetached(ctx context.Context, event audit.Event) {
	ctx = context.WithoutCancel(ctx)
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		return tx.Audit.RecordTx(ctx, event)
	})
	if err != nil {
		slog.Error("failed to record MFA failure audit event",
			slog.String("action", event.Action),
			slog.Any("error", err),
		)
	}
}

// mfaChallenge is the sealed two-step login payload. It carries only data the
// server re-validates: the user, the tenant, the token version the first
// factor saw (a password change or logout-all voids the challenge), the
// first-factor methods, and an expiry. The nonce keys the cache entry that
// makes it single use.
type mfaChallenge struct {
	Version      int       `json:"v"`
	UserID       uuid.UUID `json:"uid"`
	TenantID     uuid.UUID `json:"tid"`
	TokenVersion int32     `json:"tv"`
	AMR          []string  `json:"amr"`
	ExpiresAt    int64     `json:"exp"`
	Nonce        string    `json:"nonce"`
}

func (s *mfaService) BeginChallenge(ctx context.Context, userID uuid.UUID, method string) (string, error) {
	if method != AMRPassword && method != AMRGoogle {
		return "", apperror.NewInternal("unsupported MFA first factor")
	}
	if _, ok := s.Cache.(cache.CompareDeleter); !ok || s.Sealer == nil {
		return "", loginLockoutUnavailable(errors.New("MFA challenge store is not configured"))
	}
	user, err := s.Stores.Users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return "", accountNotActive()
		}
		return "", apperror.NewInternal("failed to load user")
	}
	nonce, err := newNonce()
	if err != nil {
		return "", apperror.NewInternal("failed to generate MFA challenge")
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return "", apperror.NewInternal("tenant context is required")
	}
	payload, err := json.Marshal(mfaChallenge{
		Version:      mfaChallengeVersion,
		UserID:       userID,
		TenantID:     tenantID,
		TokenVersion: user.TokenVersion,
		AMR:          []string{method},
		ExpiresAt:    s.now().Add(mfaChallengeTTL).Unix(),
		Nonce:        nonce,
	})
	if err != nil {
		return "", apperror.NewInternal("failed to encode MFA challenge")
	}
	sealed, err := s.Sealer.Seal(payload)
	if err != nil {
		return "", apperror.NewInternal("failed to seal MFA challenge")
	}
	stored, err := s.Cache.SetNX(ctx, mfaChallengeKey(tenantID, nonce), []byte(nonce), mfaChallengeTTL)
	if err != nil {
		return "", loginLockoutUnavailable(err)
	}
	if !stored {
		return "", apperror.NewInternal("failed to store MFA challenge")
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *mfaService) Verify(ctx context.Context, challenge, code string) (*MFALogin, error) {
	payload, err := s.openChallenge(challenge)
	if err != nil {
		return nil, err
	}
	if payload.TenantID != tenant.IDFromContext(ctx) {
		s.recordMFAFailure(ctx, payload.UserID, "challenge_invalid")
		return nil, mfaChallengeInvalid()
	}
	if s.now().Unix() > payload.ExpiresAt {
		s.recordMFAFailure(ctx, payload.UserID, "challenge_invalid")
		return nil, mfaChallengeInvalid()
	}

	user, err := s.Stores.Users.GetByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, mfaChallengeInvalid()
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	matched, attempts, err := s.checkCode(ctx, user.ID, code)
	if err != nil {
		s.auditSecondFactorFailure(ctx, user.ID, attempts, err)
		return nil, err
	}

	amr := append(append([]string(nil), payload.AMR...), mfaMethodAMR(matched.method))
	var result *MFALogin
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		// Claim the code first: a wrong or replayed code must not burn the
		// challenge, and only the request that wins the code race may proceed.
		if err := s.consumeCode(ctx, tx.Stores, user.ID, matched); err != nil {
			return err
		}
		if err := s.consumeChallenge(ctx, payload); err != nil {
			return err
		}
		// The session insert requires the token version the first factor saw.
		tokens, err := s.Sessions.IssueTx(ctx, tx.Stores, user.ID, payload.TokenVersion, amr)
		if err != nil {
			return err
		}
		member, err := tx.Memberships.GetActive(ctx, user.ID)
		if err != nil {
			return accountNotActive()
		}
		event := userEvent(ctx, "auth.mfa_login", user.ID, map[string]any{
			"method": matched.auditMethod(),
			"amr":    amr,
		})
		event.TenantID = member.Membership.TenantID
		event.ActorID = user.ID
		event.ActorRole = member.Membership.Role
		event.ActorType = audit.ActorTypeUser
		if err := checkAudit(tx.Audit.RecordTx(ctx, event)); err != nil {
			return err
		}
		result = &MFALogin{Tokens: tokens, Member: member}
		return nil
	})
	if err != nil {
		// The claim or the challenge failed inside the transaction, which
		// rolled back; record the failed attempt in its own transaction.
		s.auditSecondFactorFailure(ctx, user.ID, 0, err)
		return nil, err
	}
	clearMFAAttempts(ctx, s.Cache, user.ID)
	clearLoginAttempts(ctx, s.Cache, user.Email)
	return result, nil
}

func (s *mfaService) openChallenge(challenge string) (*mfaChallenge, error) {
	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil {
		return nil, mfaChallengeInvalid()
	}
	plain, err := s.Sealer.Open(raw)
	if err != nil {
		return nil, mfaChallengeInvalid()
	}
	var payload mfaChallenge
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, mfaChallengeInvalid()
	}
	if payload.Version != mfaChallengeVersion || payload.UserID == uuid.Nil ||
		payload.TenantID == uuid.Nil || payload.Nonce == "" || len(payload.AMR) == 0 {
		return nil, mfaChallengeInvalid()
	}
	return &payload, nil
}

// consumeChallenge deletes the cache entry only when its value still is this
// challenge's nonce. Two concurrent verifications of one challenge: exactly
// one CompareAndDelete succeeds. BeginChallenge guarantees the cache supports
// it; a cache failure is 503, not an invalid challenge.
func (s *mfaService) consumeChallenge(ctx context.Context, payload *mfaChallenge) error {
	cd, ok := s.Cache.(cache.CompareDeleter)
	if !ok {
		return loginLockoutUnavailable(errors.New("MFA challenge store cannot compare-and-delete"))
	}
	consumed, err := cd.CompareAndDelete(ctx, mfaChallengeKey(payload.TenantID, payload.Nonce), []byte(payload.Nonce))
	if err != nil {
		return loginLockoutUnavailable(err)
	}
	if !consumed {
		return mfaChallengeInvalid()
	}
	return nil
}

func mfaChallengeKey(tenantID uuid.UUID, nonce string) string {
	return fmt.Sprintf("mfa:challenge:%s:%s", tenantID, nonce)
}

func mfaMethodAMR(method string) string {
	if method == "recovery_code" {
		return AMRRecoveryCode
	}
	return AMROTP
}

func newNonce() (string, error) {
	b := make([]byte, mfaChallengeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newRecoveryCodes returns the plaintext codes (shown once) and their storage
// hashes. Ten codes of 120 random bits each; normalization removes separators.
func newRecoveryCodes() (plain []string, hashes [][]byte, err error) {
	plain = make([]string, mfaRecoveryCodeCount)
	hashes = make([][]byte, mfaRecoveryCodeCount)
	for i := range plain {
		b := make([]byte, mfaRecoveryCodeBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		code := recoveryEncoding.EncodeToString(b)
		// 15 bytes encode to 24 base32 characters, shown as six groups of four.
		plain[i] = code[0:4] + "-" + code[4:8] + "-" + code[8:12] + "-" +
			code[12:16] + "-" + code[16:20] + "-" + code[20:24]
		hashes[i] = hashToken(normalizeRecoveryCode(plain[i]))
	}
	return plain, hashes, nil
}

func normalizeRecoveryCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}
