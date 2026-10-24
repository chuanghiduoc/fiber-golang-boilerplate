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
// EffectiveID falls back to the default tenant only for pre-tenant call sites
// and tests; production requests always carry an explicit context value.
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
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" &&
		strings.Contains(pgErr.Message, "last active admin") {
		return apperror.NewConflict("cannot remove the last active admin")
	}
	return err
}
