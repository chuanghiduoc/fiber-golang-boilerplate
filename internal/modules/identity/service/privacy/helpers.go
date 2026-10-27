package privacy

import (
	"context"

	"github.com/google/uuid"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
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
func memberPage(members []Member, pageSize int) (page []Member, more bool, pageErr error) {
	return core.MemberPage(members, pageSize)
}
func userWriteError(err error, notFoundDetail, internalDetail string) error {
	return core.UserWriteError(err, notFoundDetail, internalDetail)
}
func invalidateSessions(ctx context.Context, sessions session.Store, userID uuid.UUID) {
	core.InvalidateSessions(ctx, sessions, userID)
}
func ensureNotLastOwner(ctx context.Context, memberships MembershipRepository, detail string) error {
	return core.EnsureNotLastOwner(ctx, memberships, detail)
}
func EnsureCanManageMember(ctx context.Context, stores Stores, targetUserID uuid.UUID) error {
	return membership.EnsureCanManageMember(ctx, stores, targetUserID)
}

var _ = []any{
	truncate, clientIPFromContext, userAgentFromContext, emailKey, newBanMarker, removalBanMarker,
	hashToken, newOpaqueToken, tokenLink, withLinkTenantSlug, buildCursor, auditWriteError,
	checkAudit, userEvent, memberPage, userWriteError, invalidateSessions, ensureNotLastOwner,
	EnsureCanManageMember,
}
