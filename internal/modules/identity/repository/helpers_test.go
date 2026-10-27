package repository

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// TestWrapErrMapsLastOwnerBySQLState proves the repository recognizes the
// trigger's dedicated SQLSTATE, not its human-readable message.
func TestWrapErrMapsLastOwnerBySQLState(t *testing.T) {
	cause := &pgconn.PgError{Code: sqlStateLastOwner, Message: "anything at all"}
	err := wrapErr(cause)

	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("wrapErr() = %v, want an AppError", err)
	}
	if appErr.Code != string(identitycatalog.CodeUserLastAdmin) {
		t.Fatalf("code = %q, want %q", appErr.Code, identitycatalog.CodeUserLastAdmin)
	}

	// The old message-based mapping must not fire for unrelated failures.
	other := &pgconn.PgError{Code: sqlStateDeadlockDetected, Message: "cannot remove the last owner"}
	if got := wrapErr(other); errors.As(got, &appErr) && appErr.Code == string(identitycatalog.CodeUserLastAdmin) {
		t.Fatal("a non-last-owner SQLSTATE must not map to user.last_admin")
	}
}
