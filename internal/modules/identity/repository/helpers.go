// Package repository is identity's Postgres driven adapter: it
// implements the service ports over sqlc and maps generated rows to service
// types, so services never see sqlc or pgx.
package repository

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// tenantID returns the request tenant every tenant-owned query must scope to.
// EffectiveID falls back to the default tenant only for background jobs,
// internal callers, and tests; HTTP requests resolve an explicit tenant (and
// production requires X-Tenant-Slug).
func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

const (
	sqlStateRaiseException   = "P0001"
	sqlStateDeadlockDetected = "40P01"
	sqlStateUniqueViolation  = "23505"
)

// wrapErr translates pgx errors to app-level sentinel errors. The repository
// is the only layer that knows about database driver errors.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == sqlStateRaiseException && strings.Contains(pgErr.Message, "last owner"):
			// protect_last_owner trigger: the database-level last-owner guard.
			return apperror.NewCode(apperror.CodeUserLastAdmin, "cannot remove the last active owner")
		case pgErr.Code == sqlStateDeadlockDetected:
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

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func optionalTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return timestamptz(*t)
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

func pgInt8(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: true}
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
