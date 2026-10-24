package service

import (
	"log/slog"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// auditWriteError maps an audit persistence failure to the public 503 code.
// The caller returns it from inside a transaction callback, so the mutation
// rolls back with the failed audit row.
func auditWriteError(err error) error {
	slog.Error("failed to write transactional audit event", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
}
