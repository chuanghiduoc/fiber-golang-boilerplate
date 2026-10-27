package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	authn "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	membership "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	mfa "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/mfa"
	tenancy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
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
func hashPassword(password string) (string, error) { return core.HashPassword(password) }
func verifyPassword(hash, password string) (ok, needsRehash bool) {
	return core.VerifyPassword(hash, password)
}
func userRegisteredEvent(member *Member) (events.Event, error) {
	return authn.UserRegisteredEvent(member)
}
func publishRegisteredTx(ctx context.Context, publisher EventPublisher, member *Member) error {
	return authn.PublishRegisteredTx(ctx, publisher, member)
}
func withAMR(amr []string, method string) []string {
	for _, value := range amr {
		if value == method {
			return append([]string(nil), amr...)
		}
	}
	return append(append([]string(nil), amr...), method)
}
func loginAttemptKeys(ctx context.Context, email string) (source, account string) {
	return authn.LoginAttemptKeys(ctx, email)
}
func clearLoginAttempts(ctx context.Context, store cache.Cache, email string) {
	authn.ClearLoginAttempts(ctx, store, email)
}
func passkeyUserID(id uuid.UUID) []byte { return mfa.PasskeyUserID(id) }
func newRecoveryCodes() (plain []string, hashes [][]byte, err error) {
	return mfa.NewRecoveryCodes()
}
func normalizeRecoveryCode(code string) string { return mfa.NormalizeRecoveryCode(code) }
func deleteAccount(ctx context.Context, st Stores, id uuid.UUID) error {
	return authn.DeleteAccount(ctx, st, id)
}

type mfaService = mfa.MFAServiceImpl

const (
	maxLoginAttempts        = 5
	maxMFAAttempts          = 5
	maxAccountLoginFailures = 100
	lockoutDuration         = 15 * time.Minute
	refreshRotationGrace    = 10 * time.Second
	mfaReauthSessionMaxAge  = 15 * time.Minute
	mfaChallengeTTL         = 5 * time.Minute
	mfaChallengeVersion     = 1
	mfaRecoveryCodeCount    = 10
	mfaRecoveryCodeBytes    = 15
	mfaChallengeBytes       = 16
	mfaEnrollmentTTL        = 15 * time.Minute
	mfaDefaultIssuer        = "Fiber Golang Boilerplate"
	passkeyDefaultName      = "Passkey"
	defaultTenantLocale     = "en"
	defaultTenantTimezone   = "UTC"
	tokenPurgeBatchSize     = 1000
	tokenPurgeTableBudget   = time.Minute
	roleRankOwner           = membership.RoleRankOwner
	roleRankAdmin           = membership.RoleRankAdmin
	roleRankMember          = membership.RoleRankMember
	argon2MemoryKiB         = core.Argon2MemoryKiB
	argon2Iterations        = core.Argon2Iterations
	argon2Parallelism       = core.Argon2Parallelism
	argon2SaltLength        = core.Argon2SaltLength
	argon2KeyLength         = core.Argon2KeyLength
	argon2Version           = core.Argon2Version
)

func roleRank(role string) int           { return membership.RoleRank(role) }
func effectiveRoleRank(role *Role) int   { return membership.EffectiveRoleRank(role) }
func highestRoleRank(roles []string) int { return membership.HighestRoleRank(roles) }
func canManageRank(callerRank, targetRank int) bool {
	return membership.CanManageRank(callerRank, targetRank)
}
func canGrantRank(callerRank, roleRank int) bool {
	return membership.CanGrantRank(callerRank, roleRank)
}
func highestEffectiveRank(ctx context.Context, roles RoleRepository, roleKeys []string) (int, error) {
	return membership.HighestEffectiveRank(ctx, roles, roleKeys)
}
func authorizeGrant(ctx context.Context, permissions []string) error {
	return membership.AuthorizeGrant(ctx, permissions)
}
func normalizeNewTenant(in *NewTenant) error { return tenancy.NormalizeNewTenant(in) }
func deletionSchedule(current *Tenant, to TenantStatus) *time.Time {
	return tenancy.DeletionSchedule(current, to)
}

var _ = []any{
	truncate, clientIPFromContext, userAgentFromContext, emailKey, newBanMarker, removalBanMarker,
	hashToken, newOpaqueToken, tokenLink, withLinkTenantSlug, buildCursor, auditWriteError,
	checkAudit, userEvent, hashPassword, verifyPassword, userRegisteredEvent, publishRegisteredTx,
	withAMR, loginAttemptKeys, clearLoginAttempts, passkeyUserID, newRecoveryCodes,
	normalizeRecoveryCode, deleteAccount, roleRank, effectiveRoleRank, highestRoleRank,
	canManageRank, canGrantRank, highestEffectiveRank, authorizeGrant, normalizeNewTenant,
	deletionSchedule,
}
