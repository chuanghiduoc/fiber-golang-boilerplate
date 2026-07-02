package repository

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Cursor carries decoded cursor-pagination parameters into repository queries.
// Limit should already include the extra row used to detect the next page.
type Cursor struct {
	HasCursor bool
	CreatedAt time.Time
	ID        int64
	Limit     int32
}

func (c Cursor) createdAt() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: c.CreatedAt, Valid: c.HasCursor}
}

// wrapErr translates pgx errors to app-level sentinel errors.
// Repository is the only layer that should know about database driver errors.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	return err
}

// IsUniqueViolation checks whether the error is a PostgreSQL unique constraint violation (23505).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
