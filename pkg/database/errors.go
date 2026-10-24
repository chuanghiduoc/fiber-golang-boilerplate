package database

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsUniqueViolation checks whether the error is a PostgreSQL unique
// constraint violation (23505). It lives here so repositories in every module
// can share the check without importing each other.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
