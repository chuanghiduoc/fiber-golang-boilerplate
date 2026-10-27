package membership

import (
	"context"

	"github.com/google/uuid"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func buildCursor(limit int, startingAfter string) (cur pagination.DBCursor, pageSize int, err error) {
	return core.BuildCursor(limit, startingAfter)
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
func auditWriteError(err error) error { return core.AuditWriteError(err) }
func checkAudit(err error) error      { return core.CheckAudit(err) }
func userEvent(ctx context.Context, action string, userID uuid.UUID, metadata map[string]any) audit.Event {
	return core.UserEvent(ctx, action, userID, metadata)
}
func newBanMarker(bans BanHasher, email string) (BanMarker, error) {
	return core.NewBanMarker(bans, email)
}
func removalBanMarker(bans BanHasher, membership *Membership, email string) (BanMarker, error) {
	return core.RemovalBanMarker(bans, membership, email)
}
func hashToken(token string) []byte                          { return core.HashToken(token) }
func newOpaqueToken() (plain string, hash []byte, err error) { return core.NewOpaqueToken() }
func tokenLink(ctx context.Context, baseURL, path, token string) string {
	return core.TokenLink(ctx, baseURL, path, token)
}
func withLinkTenantSlug(ctx context.Context, tenants TenantRepository) (context.Context, error) {
	return core.WithLinkTenantSlug(ctx, tenants)
}
func ValidatePasswordForEmail(password, email, field string) error {
	return core.ValidatePasswordForEmail(password, email, field)
}
func hashPassword(password string) (string, error) { return core.HashPassword(password) }
func verifyPassword(hash, password string) (ok, needsRehash bool) {
	return core.VerifyPassword(hash, password)
}
func truncate(s string, n int) string { return core.Truncate(s, n) }

func ensureEmailNotBanned(ctx context.Context, st Stores, email string) error {
	return core.EnsureEmailNotBanned(ctx, st, email)
}
func accountNotActive() error { return core.AccountNotActive() }
func createAccount(ctx context.Context, st Stores, user NewUser, hash, role string) (*Member, error) {
	return core.CreateAccount(ctx, st, user, hash, role)
}

var _ = []any{
	buildCursor, memberPage, userWriteError, invalidateSessions, ensureNotLastOwner, auditWriteError,
	checkAudit, userEvent, newBanMarker, removalBanMarker, hashToken, newOpaqueToken, tokenLink,
	withLinkTenantSlug, emailTaken, emailUnavailable, accountNotActive, ensureEmailNotBanned,
	createAccount, ValidatePasswordForEmail, hashPassword, verifyPassword, truncate,
}

func emailTaken() error       { return core.EmailTaken() }
func emailUnavailable() error { return core.EmailUnavailable() }
