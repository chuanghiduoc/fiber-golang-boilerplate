package database

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
