package authn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// A password built from the address it belongs to is refused where the
// service knows both (registration).
func TestRegister_RejectsPasswordContainingEmailLocalPart(t *testing.T) {
	db := fakes.New()
	svc := newTestUserService(db)

	_, err := svc.Register(fakes.TenantCtx(), RegisterInput{
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
	err := core.ValidatePasswordForEmail("correct horse battery", "alice@example.com", "password")
	if err != nil {
		t.Errorf("passwordFromEmail returned %v", err)
	}
}

func TestResetPassword_RejectsPasswordFromEmailWithoutConsumingToken(t *testing.T) {
	db := fakes.New()
	userID := fakes.TestID(77)
	db.Users[userID] = &core.User{
		ID: userID, Email: "alice@example.com", Name: "Alice", Status: core.UserStatusActive,
	}
	token, tokenHash, err := newOpaqueToken()
	if err != nil {
		t.Fatalf("newOpaqueToken: %v", err)
	}
	created, err := db.Stores().Resets.Create(context.Background(), userID, "alice@example.com", tokenHash, time.Now().Add(time.Hour))
	if err != nil || !created {
		t.Fatalf("create reset token: created=%v err=%v", created, err)
	}
	svc := NewPasswordResetService(ResetDeps{Stores: db.Stores(), Tx: fakes.Tx{DB: db}})

	err = svc.ResetPassword(context.Background(), ResetPasswordInput{
		Token: token, Password: strings.Join([]string{"alice", "fixture", "guess"}, "-"),
	})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("ResetPassword error = %v, want an AppError", err)
	}
	if resolvedStatus(appErr) != 422 {
		t.Errorf("status = %d, want 422", resolvedStatus(appErr))
	}
	if len(appErr.Errors) != 1 || appErr.Errors[0].Code != validator.CodePasswordFromEmail {
		t.Errorf("field errors = %+v, want %q", appErr.Errors, validator.CodePasswordFromEmail)
	}
	if _, ok := db.ResetTokens[string(tokenHash)]; !ok {
		t.Fatal("a policy rejection must not consume the reset token")
	}
}
