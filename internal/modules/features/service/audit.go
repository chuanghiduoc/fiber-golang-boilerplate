package service

import (
	"errors"
	"log/slog"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// checkAudit maps a failed Tier 1 audit write to the public 503 code. The
// caller returns it from inside a transaction callback, so the flag mutation
// rolls back with the failed audit row.
func checkAudit(err error) error {
	if err == nil {
		return nil
	}
	slog.Error("failed to write transactional audit event", slog.Any("error", err))
	return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
}

// flagWriteError preserves business errors and turns persistence failures into
// a public 500 while keeping the cause for the error log.
func flagWriteError(err error) error {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return err
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return apperror.NewNotFound("feature flag not found")
	}
	return apperror.Internal(err, "failed to save feature flag")
}

// targetWriteError preserves business errors and turns persistence failures
// into a public 500 while keeping the cause for the error log.
func targetWriteError(err error) error {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return err
	}
	if errors.Is(err, apperror.ErrNotFound) {
		return apperror.NewNotFound("feature flag target not found")
	}
	return apperror.Internal(err, "failed to save feature flag target")
}
