// Package repository is identity's Postgres driven adapter: it
// implements the service ports over sqlc and maps generated rows to service
// types, so services never see sqlc or pgx.
package repository

import (
	"errors"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const (
	sqlStateRaiseException   = "P0001"
	sqlStateDeadlockDetected = "40P01"
	sqlStateUniqueViolation  = "23505"
	// sqlStateLastOwner is raised by protect_last_owner, which guards the last
	// active owner of an operational tenant. It is deliberately distinct from
	// the generic raise_exception code so the repository maps by SQLSTATE
	// instead of matching the message text.
	sqlStateLastOwner = "LO001"
)

// wrapErr translates pgx errors to app-level sentinel errors. The repository
// is the only layer that knows about database driver errors.
func wrapErr(err error) error {
	if mapped := database.MapError(err); mapped != err {
		return mapped
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlStateLastOwner:
			// protect_last_owner trigger: the database-level last-owner guard.
			return apperror.NewCode(identitycatalog.CodeUserLastAdmin, "cannot remove the last active owner")
		case sqlStateDeadlockDetected:
			// PostgreSQL aborted one side of a lock cycle (for example a
			// concurrent admin change made outside the service lock). The
			// other side committed; the caller can retry.
			return apperror.NewConflict("the account was modified concurrently, please retry")
		}
	}
	return err
}

// uniqueConstraint returns the violated constraint (or unique index) name of
// a unique violation.
func uniqueConstraint(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlStateUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// optionalText stores "" as NULL.
func optionalText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// ipAddr parses a client IP for an INET column; anything that is not an IP
// address is stored as NULL rather than failing the sign-in.
func ipAddr(s string) *netip.Addr {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return &addr
}

func isNotFound(err error) bool {
	return errors.Is(err, apperror.ErrNotFound)
}

func ipString(addr *netip.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}
