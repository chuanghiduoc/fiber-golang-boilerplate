package core

import (
	"context"
	"log/slog"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// AuditWriteError maps an audit persistence failure to the public 503 code.
// The caller returns it from inside a transaction callback, so the mutation
// rolls back with the failed audit row.
func AuditWriteError(err error) error {
	slog.Error("failed to write transactional audit event", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
}

// CheckAudit maps the result of a Tier 1 TxRepos.Audit.RecordTx call: nil
// stays nil, a failure becomes the public 503 audit-unavailable error.
// Returning it from an InTx callback rolls the audited mutation back. Call
// sites keep the RecordTx call visible next to the action they record.
func CheckAudit(err error) error {
	if err != nil {
		return AuditWriteError(err)
	}
	return nil
}

// UserEvent is a service audit event about one user account.
func UserEvent(ctx context.Context, action string, userID uuid.UUID, metadata map[string]any) audit.Event {
	event := audit.NewServiceEvent(ctx, action, "user")
	event.ResourceID = userID.String()
	event.Metadata = metadata
	return event
}
