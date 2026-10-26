package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestWrapErrMapsMembershipForeignKeyToNotFound(t *testing.T) {
	cases := map[string]error{
		"missing membership on insert": &pgconn.PgError{
			Code: "23503", ConstraintName: "fk_notifications_tenant_user",
		},
		"restrict violation": &pgconn.PgError{
			Code: "23001", ConstraintName: "fk_notification_preferences_tenant_user",
		},
		"wrapped": fmt.Errorf("create notification: %w", &pgconn.PgError{
			Code: "23503", ConstraintName: "fk_notifications_tenant_user",
		}),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(wrapErr(err), apperror.ErrNotFound) {
				t.Fatalf("wrapErr(%v) = %v, want ErrNotFound", err, wrapErr(err))
			}
		})
	}
}

func TestWrapErrLeavesOtherErrorsAlone(t *testing.T) {
	original := errors.New("connection refused")
	if got := wrapErr(original); !errors.Is(got, original) {
		t.Fatalf("wrapErr(%v) = %v, want it unchanged", original, got)
	}
}
