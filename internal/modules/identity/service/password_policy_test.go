package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// A password built from the address it belongs to is refused where the
// service knows both (registration).
func TestRegister_RejectsPasswordContainingEmailLocalPart(t *testing.T) {
	db := newMemDB()
	svc := newTestUserService(db)

	_, err := svc.Register(testTenantCtx(), RegisterInput{
		Email: "alice@example.com", Password: strings.Join([]string{"alice", "fixture", "guess"}, "-"), Name: "Alice",
	})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("Register error = %v, want an AppError", err)
	}
	if resolvedStatus(appErr) != 422 {
		t.Errorf("status = %d, want 422", resolvedStatus(appErr))
	}
	if len(appErr.Errors) != 1 || appErr.Errors[0].Code != validator.CodePasswordFromEmail {
		t.Errorf("field errors = %+v, want one %q", appErr.Errors, validator.CodePasswordFromEmail)
	}
}

func TestPasswordFromEmail_AllowsUnrelatedPassword(t *testing.T) {
	err := ValidatePasswordForEmail("correct horse battery", "alice@example.com", "password")
	if err != nil {
		t.Errorf("passwordFromEmail returned %v", err)
	}
}
