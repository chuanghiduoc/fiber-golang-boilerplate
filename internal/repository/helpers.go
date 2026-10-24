package repository

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// Cursor carries decoded cursor-pagination parameters into repository queries.
// It aliases the neutral pagination type so global repositories and module
// repositories share one cursor representation.
type Cursor = pagination.DBCursor

func cursorCreatedAt(c Cursor) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: c.CreatedAt, Valid: c.HasCursor}
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

// IsUniqueViolation checks whether the error is a PostgreSQL unique constraint violation (23505).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
