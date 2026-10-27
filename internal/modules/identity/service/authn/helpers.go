package authn

import (
	"context"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func truncate(s string, n int) string                 { return core.Truncate(s, n) }
func clientIPFromContext(ctx context.Context) string  { return core.ClientIPFromContext(ctx) }
func userAgentFromContext(ctx context.Context) string { return core.UserAgentFromContext(ctx) }
func emailKey(prefix, email string) string            { return core.EmailKey(prefix, email) }
func newBanMarker(bans BanHasher, email string) (BanMarker, error) {
	return core.NewBanMarker(bans, email)
}
func removalBanMarker(bans BanHasher, member *Membership, email string) (BanMarker, error) {
	return core.RemovalBanMarker(bans, member, email)
}
func hashToken(token string) []byte                          { return core.HashToken(token) }
func newOpaqueToken() (plain string, hash []byte, err error) { return core.NewOpaqueToken() }
func tokenLink(ctx context.Context, baseURL, path, token string) string {
	return core.TokenLink(ctx, baseURL, path, token)
}
func withLinkTenantSlug(ctx context.Context, tenants TenantRepository) (context.Context, error) {
	return core.WithLinkTenantSlug(ctx, tenants)
}
func buildCursor(limit int, startingAfter string) (cur pagination.DBCursor, pageSize int, err error) {
	return core.BuildCursor(limit, startingAfter)
}
func auditWriteError(err error) error { return core.AuditWriteError(err) }
func checkAudit(err error) error      { return core.CheckAudit(err) }
func userEvent(ctx context.Context, action string, userID uuid.UUID, metadata map[string]any) audit.Event {
	return core.UserEvent(ctx, action, userID, metadata)
}
func emailTaken() error       { return core.EmailTaken() }
func emailUnavailable() error { return core.EmailUnavailable() }
func accountNotActive() error { return core.AccountNotActive() }
func ensureEmailNotBanned(ctx context.Context, st Stores, email string) error {
	return core.EnsureEmailNotBanned(ctx, st, email)
}
func createAccount(ctx context.Context, st Stores, user NewUser, hash, role string) (*Member, error) {
	return core.CreateAccount(ctx, st, user, hash, role)
}
func ValidatePasswordForEmail(password, email, field string) error {
	return core.ValidatePasswordForEmail(password, email, field)
}
func hashPassword(password string) (string, error) { return core.HashPassword(password) }
func verifyPassword(hash, password string) (ok, needsRehash bool) {
	return core.VerifyPassword(hash, password)
}
func spendDummyPasswordWork(password string) { core.SpendDummyPasswordWork(password) }
func ensureNotLastOwner(ctx context.Context, memberships MembershipRepository, detail string) error {
	return core.EnsureNotLastOwner(ctx, memberships, detail)
}
func EnsureCanManageMember(ctx context.Context, stores Stores, targetUserID uuid.UUID) error {
	return membership.EnsureCanManageMember(ctx, stores, targetUserID)
}
func CleanupStaleSessions(ctx context.Context, sessions SessionRepository, userID uuid.UUID, why RevokeReason) {
	cleanupStaleSessions(ctx, sessions, userID, why)
}
func CleanupOtherSessions(ctx context.Context, sessions SessionRepository, userID, keepID uuid.UUID, why RevokeReason) {
	cleanupOtherSessions(ctx, sessions, userID, keepID, why)
}
func LoginLockoutUnavailable(err error) error { return loginLockoutUnavailable(err) }
func CurrentPasswordIncorrect() error         { return currentPasswordIncorrect() }
func LoginAttemptKeys(ctx context.Context, email string) (source, account string) {
	return loginAttemptKeys(ctx, email)
}
func DeleteAccount(ctx context.Context, st Stores, id uuid.UUID) error {
	return deleteAccount(ctx, st, id)
}

const MaxMFAAttempts = maxMFAAttempts

func ReserveReauthAttempt(ctx context.Context, store cache.Cache, userID uuid.UUID) error {
	return reserveReauthAttempt(ctx, store, userID)
}
func ClearReauthAttempts(ctx context.Context, store cache.Cache, userID uuid.UUID) {
	clearReauthAttempts(ctx, store, userID)
}
func ClearMFAAttempts(ctx context.Context, store cache.Cache, userID uuid.UUID) {
	clearMFAAttempts(ctx, store, userID)
}
func ReserveMFAAttempt(ctx context.Context, store cache.Cache, userID uuid.UUID) (int64, error) {
	return reserveMFAAttempt(ctx, store, userID)
}
func ClearLoginAttempts(ctx context.Context, store cache.Cache, email string) {
	clearLoginAttempts(ctx, store, email)
}

var _ = []any{
	truncate, clientIPFromContext, userAgentFromContext, emailKey, newBanMarker, removalBanMarker,
	hashToken, newOpaqueToken, tokenLink, withLinkTenantSlug, buildCursor, auditWriteError,
	checkAudit, userEvent, emailTaken, emailUnavailable, accountNotActive, ensureEmailNotBanned,
	createAccount, ValidatePasswordForEmail, hashPassword, verifyPassword, spendDummyPasswordWork,
	ensureNotLastOwner, EnsureCanManageMember, CleanupStaleSessions, CleanupOtherSessions,
	LoginLockoutUnavailable, CurrentPasswordIncorrect, LoginAttemptKeys, DeleteAccount,
	ReserveReauthAttempt, ClearReauthAttempts, ClearMFAAttempts, ReserveMFAAttempt, ClearLoginAttempts,
}
