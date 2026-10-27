package database

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// SQLSTATE codes shared by repositories.
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
	// sqlStateRestrictViolation is what PostgreSQL 18+ reports when deleting a
	// row still referenced through an ON DELETE RESTRICT foreign key; older
	// versions report foreign_key_violation (23503) for the same failure.
	sqlStateRestrictViolation = "23001"
)

// IsUniqueViolation checks whether the error is a PostgreSQL unique
// constraint violation (23505). It lives here so repositories in every module
// can share the check without importing each other.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlStateUniqueViolation
}

// ForeignKeyViolation returns the constraint name when err is a foreign key
// failure: a missing referenced row on insert/update (23503) or a delete
// blocked by ON DELETE RESTRICT (23503 before PostgreSQL 18, 23001 since).
func ForeignKeyViolation(err error) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", false
	}
	switch pgErr.Code {
	case sqlStateForeignKeyViolation, sqlStateRestrictViolation:
		return pgErr.ConstraintName, true
	}
	return "", false
}

// MapError translates the driver errors every repository shares: no row maps
// to apperror.ErrNotFound (a 404). Repositories call it first and then add
// their own SQLSTATE-specific cases (for example a unique violation).
func MapError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	return err
}
