package tenancy

import (
	"context"

	"github.com/google/uuid"

	core "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func truncate(s string, n int) string                 { return core.Truncate(s, n) }
func clientIPFromContext(ctx context.Context) string  { return core.ClientIPFromContext(ctx) }
func userAgentFromContext(ctx context.Context) string { return core.UserAgentFromContext(ctx) }
func emailKey(prefix, email string) string            { return core.EmailKey(prefix, email) }
func hashToken(token string) []byte                   { return core.HashToken(token) }
func auditWriteError(err error) error                 { return core.AuditWriteError(err) }
func checkAudit(err error) error                      { return core.CheckAudit(err) }
func userEvent(ctx context.Context, action string, userID uuid.UUID, metadata map[string]any) audit.Event {
	return core.UserEvent(ctx, action, userID, metadata)
}
func buildCursor(limit int, startingAfter string) (cur pagination.DBCursor, pageSize int, err error) {
	return core.BuildCursor(limit, startingAfter)
}
func memberPage(members []Member, pageSize int) (page []Member, more bool, pageErr error) {
	return core.MemberPage(members, pageSize)
}
func invalidateSessions(ctx context.Context, sessions session.Store, userID uuid.UUID) {
	core.InvalidateSessions(ctx, sessions, userID)
}

var _ = []any{
	truncate, clientIPFromContext, userAgentFromContext, emailKey, hashToken, auditWriteError,
	checkAudit, userEvent, buildCursor, memberPage, invalidateSessions,
}
