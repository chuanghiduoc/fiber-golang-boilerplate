package features

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestRequireReturnsFeatureDisabled(t *testing.T) {
	flags := NewEvaluator(Options{Env: map[Key]bool{KeyUserRegistration: false}})

	err := Require(context.Background(), flags, KeyUserRegistration, Subject{TenantID: 1})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("Require() = %v, want *apperror.AppError", err)
	}
	if appErr.Code != string(apperror.CodeFeatureDisabled) || appErr.Status != http.StatusNotFound {
		t.Fatalf("Require() = %s/%d, want feature.disabled/404", appErr.Code, appErr.Status)
	}
	if err := Require(context.Background(), flags, KeyFileUploads, Subject{TenantID: 1}); err != nil {
		t.Fatalf("enabled flag returned %v", err)
	}
}

func TestRequireNilEvaluatorUsesCompiledDefaults(t *testing.T) {
	if err := Require(context.Background(), nil, KeyEmailVerification, Subject{}); err == nil {
		t.Fatal("default-off flag must fail closed with a nil evaluator")
	}
	if err := Require(context.Background(), nil, KeyFileUploads, Subject{}); err != nil {
		t.Fatalf("default-on flag returned %v", err)
	}
}
