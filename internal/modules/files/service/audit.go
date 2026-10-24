package service

import (
	"log/slog"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// filesAuditWriteError maps an audit persistence failure to the public 503
// code. Returning it from the transaction callback rolls the metadata mutation
// back, so an unaudited file change is never committed.
func filesAuditWriteError(err error) error {
	slog.Error("failed to write transactional audit event", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
}
