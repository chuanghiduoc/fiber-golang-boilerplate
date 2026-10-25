package service

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// auditWriteError maps an audit persistence failure to the public 503 code.
// The caller returns it from inside a transaction callback, so the mutation
// rolls back with the failed audit row.
func auditWriteError(err error) error {
	slog.Error("failed to write transactional audit event", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
}

// checkAudit maps the result of a Tier 1 TxRepos.Audit.RecordTx call: nil
// stays nil, a failure becomes the public 503 audit-unavailable error.
// Returning it from an InTx callback rolls the audited mutation back. Call
// sites keep the RecordTx call visible next to the action they record.
func checkAudit(err error) error {
	if err != nil {
		return auditWriteError(err)
	}
	return nil
}

// userEvent is a service audit event about one user account.
func userEvent(ctx context.Context, action string, userID int64, metadata map[string]any) audit.Event {
	event := audit.NewServiceEvent(ctx, action, "user")
	event.ResourceID = strconv.FormatInt(userID, 10)
	event.Metadata = metadata
	return event
}
