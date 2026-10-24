package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// tenantID returns the request tenant every repository query must scope to.
// EffectiveID falls back to the default tenant only for background jobs,
// internal callers, and tests; HTTP requests resolve an explicit tenant (and
// production requires X-Tenant-Slug).
func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

// wrapErr translates pgx errors to app-level sentinel errors.
// Repository is the only layer that should know about database driver errors.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == sqlStateRaiseException && strings.Contains(pgErr.Message, "last active admin"):
			// protect_last_admin trigger: the database-level last-admin guard.
			return apperror.NewCode(apperror.CodeUserLastAdmin, "cannot remove the last active admin")
		case pgErr.Code == sqlStateDeadlockDetected:
			// PostgreSQL aborted one side of a lock cycle (for example a
			// concurrent admin change made outside the service lock). The
			// other side committed; the caller can retry.
			return apperror.NewConflict("the account was modified concurrently, please retry")
		}
	}
	return err
}

const (
	sqlStateRaiseException   = "P0001"
	sqlStateDeadlockDetected = "40P01"
)
