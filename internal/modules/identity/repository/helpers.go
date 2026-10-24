package repository

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

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
