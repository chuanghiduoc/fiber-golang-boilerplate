package service

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
)

const (
	mfaChallengeTTL      = 5 * time.Minute
	mfaChallengeVersion  = 1
	mfaRecoveryCodeCount = 10
	mfaRecoveryCodeBytes = 10
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
	EnrollTOTP(ctx context.Context, userID, sessionID int64, currentPassword string) (*MFAEnrollment, error)
	// ConfirmTOTP verifies the first code, confirms the factor, replaces the
	// recovery codes, and returns them once. Like enrollment it requires
	// re-authentication.
	ConfirmTOTP(ctx context.Context, userID, sessionID int64, currentPassword, code string) ([]string, error)
	// Disable removes the factor and recovery codes after a TOTP or recovery
	// code proof.
	Disable(ctx context.Context, userID, sessionID int64, code string) error
	// RegenerateRecoveryCodes replaces the set after a code proof and returns
	// the new codes once.
	RegenerateRecoveryCodes(ctx context.Context, userID int64, code string) ([]string, error)
	// ResetTOTP is the platform recovery path: it removes the confirmed and
	// pending factor plus recovery codes, revokes every session, and bumps the
	// token version. It does not require the user's password or a code.
	ResetTOTP(ctx context.Context, userID int64) error
	Status(ctx context.Context, userID int64) (*MFAStatus, error)
	// Required reports whether the user has a confirmed factor.
	Required(ctx context.Context, userID int64) (bool, error)
	// BeginChallenge seals a short-lived, single-use, tenant-bound challenge
	// after the first factor (method pwd or google) succeeded.
	BeginChallenge(ctx context.Context, userID int64, method string) (string, error)
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

func (s *mfaService) Required(ctx context.Context, userID int64) (bool, error) {
	_, err := s.Stores.MFA.GetConfirmedTOTP(ctx, userID)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return false, nil
	}
	return false, apperror.NewInternal("failed to load MFA factor")
}

func (s *mfaService) Status(ctx context.Context, userID int64) (*MFAStatus, error) {
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

func (s *mfaService) EnrollTOTP(ctx context.Context, userID, sessionID int64, currentPassword string) (*MFAEnrollment, error) {
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

	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if _, err := tx.MFA.UpsertUnconfirmedTOTP(ctx, userID, string(sealed)); err != nil {
			return apperror.NewInternal("failed to store MFA factor")
		}
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
	return &MFAEnrollment{OtpauthURI: totp.URI(s.Issuer, user.Email, secret)}, nil
}

func (s *mfaService) ConfirmTOTP(ctx context.Context, userID, sessionID int64, currentPassword, code string) ([]string, error) {
	if err := s.reauthenticate(ctx, userID, sessionID, currentPassword); err != nil {
		return nil, err
	}
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate recovery codes")
	}
	ctx = database.WithRLSBypass(ctx) // Confirming MFA ends the user's other sessions in every tenant.
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
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
		// The row is locked: this is the secret a concurrent enroll cannot
		// replace between verification and the confirm update.
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
		if err := tx.MFA.DeleteRecoveryCodes(ctx, userID); err != nil {
			return apperror.NewInternal("failed to replace recovery codes")
		}
		if err := tx.MFA.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
			return apperror.NewInternal("failed to store recovery codes")
		}
		if err := tx.Sessions.RevokeOtherUserSessions(ctx, userID, sessionID, RevokeMFAChanged); err != nil {
			return apperror.NewInternal("failed to revoke other sessions")
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "auth.mfa_enabled", userID, map[string]any{
			"type":           MFAFactorTOTP,
			"recovery_codes": len(plain),
		})))
	})
	if err != nil {
		return nil, err
	}
	if s.Sessions != nil {
		s.Sessions.invalidate(ctx, userID)
	}
	return plain, nil
}

// reauthenticate proves the caller may change the account's second factor.
// A local password is checked against the bcrypt hash and a mismatch spends
// the per-user second-factor budget (a correct password does not clear it).
// A passwordless account instead needs an interactive sign-in no older than
// mfaReauthSessionMaxAge: a stolen access token alone must not bind a new
// authenticator.
func (s *mfaService) reauthenticate(ctx context.Context, userID, sessionID int64, currentPassword string) error {
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
	if err := checkMFAAttemptBudget(ctx, s.Cache, userID); err != nil {
		return err
	}
	hash, err := s.Stores.Users.PasswordHash(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return currentPasswordIncorrect()
		}
		return apperror.NewInternal("failed to get user")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(currentPassword)) != nil {
		if err := recordFailedMFAAttempt(ctx, s.Cache, userID); err != nil {
			return err
		}
		return currentPasswordIncorrect()
	}
	return nil
}

func (s *mfaService) reauthenticateSession(ctx context.Context, userID, sessionID int64) error {
	sess, err := s.Stores.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return mfaReauthRequired()
		}
		return apperror.NewInternal("failed to load session")
	}
	if sess.UserID != userID || sess.TenantID != tenant.EffectiveID(ctx) ||
		sess.RevokedAt != nil || !sess.ExpiresAt.After(s.now()) ||
		s.now().Sub(sess.CreatedAt) > mfaReauthSessionMaxAge {
		return mfaReauthRequired()
	}
	return nil
}

// ResetTOTP removes every second factor of a global user and ends all their
// sessions. The platform route calls it when a user lost both the
// authenticator and the recovery codes.
func (s *mfaService) ResetTOTP(ctx context.Context, userID int64) error {
	ctx = database.WithRLSBypass(ctx) // A platform reset ends the user's sessions in every tenant.
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if _, err := tx.Users.GetByID(ctx, userID); err != nil {
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
		if err := tx.Sessions.RevokeAllForUser(ctx, userID, RevokeMFAChanged); err != nil {
			return apperror.NewInternal("failed to revoke sessions")
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "admin.user.mfa_reset", userID, map[string]any{
			"scope": "platform",
		})))
	})
	if err != nil {
		return err
	}
	if s.Sessions != nil {
		s.Sessions.invalidate(ctx, userID)
	}
	return nil
}

func (s *mfaService) Disable(ctx context.Context, userID, sessionID int64, code string) error {
	matched, err := s.checkCode(ctx, userID, code)
	if err != nil {
		return err
	}
	ctx = database.WithRLSBypass(ctx) // Disabling MFA ends the user's other sessions in every tenant.
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
		if err := tx.Sessions.RevokeOtherUserSessions(ctx, userID, sessionID, RevokeMFAChanged); err != nil {
			return apperror.NewInternal("failed to revoke other sessions")
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "auth.mfa_disabled", userID, map[string]any{
			"method": matched.auditMethod(),
		})))
	})
	if err != nil {
		return err
	}
	clearMFAAttempts(ctx, s.Cache, userID)
	if s.Sessions != nil {
		s.Sessions.invalidate(ctx, userID)
	}
	return nil
}

func (s *mfaService) RegenerateRecoveryCodes(ctx context.Context, userID int64, code string) ([]string, error) {
	matched, err := s.checkCode(ctx, userID, code)
	if err != nil {
		return nil, err
	}
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, apperror.NewInternal("failed to generate recovery codes")
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
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "auth.mfa_recovery_codes_regenerated", userID, map[string]any{
			"method": matched.auditMethod(),
			"count":  len(plain),
		})))
	})
	if err != nil {
		return nil, err
	}
	clearMFAAttempts(ctx, s.Cache, userID)
	return plain, nil
}

// mfaCode is a verified TOTP step or recovery-code hash that still has to be
// claimed atomically.
type mfaCode struct {
	method string
	step   int64
	hash   string
}

func (c *mfaCode) auditMethod() string { return c.method }

// checkCode counts one second-factor guess against the per-user MFA budget,
// then matches code. Callers clear the budget after the code is claimed.
func (s *mfaService) checkCode(ctx context.Context, userID int64, code string) (*mfaCode, error) {
	if s.Cache == nil {
		return nil, loginLockoutUnavailable(errors.New("MFA attempt store is not configured"))
	}
	if err := reserveMFAAttempt(ctx, s.Cache, userID); err != nil {
		return nil, err
	}
	return s.matchCode(ctx, s.Stores, userID, code)
}

// matchCode checks code against the confirmed factor. A non-matching TOTP
// value is treated as a recovery code; claimCode decides whether it exists.
func (s *mfaService) matchCode(ctx context.Context, st Stores, userID int64, code string) (*mfaCode, error) {
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
func (s *mfaService) consumeCode(ctx context.Context, st Stores, userID int64, matched *mfaCode) error {
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

// mfaChallenge is the sealed two-step login payload. It carries only data the
// server re-validates: the user, the tenant, the token version the first
// factor saw (a password change or logout-all voids the challenge), the
// first-factor methods, and an expiry. The nonce keys the cache entry that
// makes it single use.
type mfaChallenge struct {
	Version      int      `json:"v"`
	UserID       int64    `json:"uid"`
	TenantID     int64    `json:"tid"`
	TokenVersion int32    `json:"tv"`
	AMR          []string `json:"amr"`
	ExpiresAt    int64    `json:"exp"`
	Nonce        string   `json:"nonce"`
}

func (s *mfaService) BeginChallenge(ctx context.Context, userID int64, method string) (string, error) {
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
	tenantID := tenant.EffectiveID(ctx)
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
	if payload.TenantID != tenant.EffectiveID(ctx) {
		return nil, mfaChallengeInvalid()
	}
	if s.now().Unix() > payload.ExpiresAt {
		return nil, mfaChallengeInvalid()
	}

	user, err := s.Stores.Users.GetByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, mfaChallengeInvalid()
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	matched, err := s.checkCode(ctx, user.ID, code)
	if err != nil {
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
		tokens, err := s.Sessions.issueTx(ctx, tx.Stores, user.ID, payload.TokenVersion, amr)
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
	if payload.Version != mfaChallengeVersion || payload.UserID <= 0 ||
		payload.TenantID <= 0 || payload.Nonce == "" || len(payload.AMR) == 0 {
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

func mfaChallengeKey(tenantID int64, nonce string) string {
	return fmt.Sprintf("mfa:challenge:%d:%s", tenantID, nonce)
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
// hashes. Ten codes of 80 random bits each; normalization removes separators.
func newRecoveryCodes() (plain, hashes []string, err error) {
	plain = make([]string, mfaRecoveryCodeCount)
	hashes = make([]string, mfaRecoveryCodeCount)
	for i := range plain {
		b := make([]byte, mfaRecoveryCodeBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		code := recoveryEncoding.EncodeToString(b)
		plain[i] = code[0:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:16]
		hashes[i] = hashToken(normalizeRecoveryCode(plain[i]))
	}
	return plain, hashes, nil
}

func normalizeRecoveryCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}
