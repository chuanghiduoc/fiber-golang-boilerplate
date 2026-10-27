package database

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestForeignKeyViolation(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{"missing referenced row", &pgconn.PgError{Code: "23503", ConstraintName: "fk_a"}, "fk_a", true},
		{"restrict on PostgreSQL 18", &pgconn.PgError{Code: "23001", ConstraintName: "fk_b"}, "fk_b", true},
		{"wrapped", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23001", ConstraintName: "fk_c"}), "fk_c", true},
		{"unique violation", &pgconn.PgError{Code: "23505", ConstraintName: "uq"}, "", false},
		{"not a pg error", errors.New("boom"), "", false},
	}
	for _, tc := range cases {
		got, ok := ForeignKeyViolation(tc.err)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: ForeignKeyViolation = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestMapErrorNoRows(t *testing.T) {
	if got := MapError(pgx.ErrNoRows); !errors.Is(got, apperror.ErrNotFound) {
		t.Fatalf("MapError(pgx.ErrNoRows) = %v, want apperror.ErrNotFound", got)
	}
	wrapped := fmt.Errorf("query: %w", pgx.ErrNoRows)
	if got := MapError(wrapped); !errors.Is(got, apperror.ErrNotFound) {
		t.Fatalf("MapError(wrapped no rows) = %v, want apperror.ErrNotFound", got)
	}
	other := errors.New("boom")
	if got := MapError(other); !errors.Is(got, other) {
		t.Fatalf("MapError(other) = %v, want the original error", got)
	}
}

func TestTimestamptzRoundTrip(t *testing.T) {
	zero := Timestamptz(time.Time{})
	if zero.Valid {
		t.Fatal("the zero time must map to NULL")
	}
	if got := TimePtr(zero); got != nil {
		t.Fatalf("TimePtr(NULL) = %v, want nil", got)
	}

	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.FixedZone("x", 3600))
	value := Timestamptz(now)
	if !value.Valid || !value.Time.Equal(now) || value.Time.Location() != time.UTC {
		t.Fatalf("Timestamptz(%v) = %+v, want a UTC value", now, value)
	}
	got := TimePtr(value)
	if got == nil || !got.Equal(now) {
		t.Fatalf("TimePtr = %v, want %v", got, now)
	}

	var nilTime *time.Time
	if null := TimestamptzPtr(nilTime); null.Valid {
		t.Fatal("TimestamptzPtr(nil) must be NULL")
	}
}
