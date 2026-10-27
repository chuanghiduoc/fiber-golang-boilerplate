package mfa

import (
	"context"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

// Local names for the shared helpers that stayed unexported before the split.
func verifyPassword(hash, password string) (ok, needsRehash bool) {
	return core.VerifyPassword(hash, password)
}

func reserveMFAAttempt(ctx context.Context, store cache.Cache, userID uuid.UUID) (int64, error) {
	return authn.ReserveMFAAttempt(ctx, store, userID)
}
func clearLoginAttempts(ctx context.Context, store cache.Cache, email string) {
	authn.ClearLoginAttempts(ctx, store, email)
}
func accountNotActive() error { return core.AccountNotActive() }

func NewRecoveryCodes() (plain []string, hashes [][]byte, err error) {
	return newRecoveryCodes()
}

func NormalizeRecoveryCode(code string) string { return normalizeRecoveryCode(code) }

func cleanupStaleSessions(ctx context.Context, sessions SessionRepository, userID uuid.UUID, why RevokeReason) {
	authn.CleanupStaleSessions(ctx, sessions, userID, why)
}
func loginLockoutUnavailable(err error) error { return authn.LoginLockoutUnavailable(err) }
func reserveReauthAttempt(ctx context.Context, store cache.Cache, userID uuid.UUID) error {
	return authn.ReserveReauthAttempt(ctx, store, userID)
}
func clearReauthAttempts(ctx context.Context, store cache.Cache, userID uuid.UUID) {
	authn.ClearReauthAttempts(ctx, store, userID)
}
func clearMFAAttempts(ctx context.Context, store cache.Cache, userID uuid.UUID) {
	authn.ClearMFAAttempts(ctx, store, userID)
}
func currentPasswordIncorrect() error { return authn.CurrentPasswordIncorrect() }
func cleanupOtherSessions(ctx context.Context, sessions SessionRepository, userID, keepID uuid.UUID, why RevokeReason) {
	authn.CleanupOtherSessions(ctx, sessions, userID, keepID, why)
}

func checkAudit(err error) error      { return core.CheckAudit(err) }
func auditWriteError(err error) error { return core.AuditWriteError(err) }
func userEvent(ctx context.Context, action string, userID uuid.UUID, metadata map[string]any) audit.Event {
	return core.UserEvent(ctx, action, userID, metadata)
}
func hashToken(token string) []byte { return core.HashToken(token) }

var _ = []any{
	verifyPassword, reserveMFAAttempt, clearLoginAttempts, accountNotActive, cleanupStaleSessions,
	loginLockoutUnavailable, reserveReauthAttempt, clearReauthAttempts, clearMFAAttempts,
	currentPasswordIncorrect, cleanupOtherSessions, checkAudit, auditWriteError, userEvent, hashToken,
}
