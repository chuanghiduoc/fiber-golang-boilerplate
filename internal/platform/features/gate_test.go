package features

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestRequireReturnsFeatureDisabled(t *testing.T) {
	flags := NewEvaluator(Options{Catalog: testCatalog, Env: map[Key]bool{KeyUserRegistration: false}})

	err := Require(context.Background(), flags, KeyUserRegistration, Subject{TenantID: testID(1)})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("Require() = %v, want *apperror.AppError", err)
	}
	_, spec := apperror.PlatformCatalog().Resolve(apperror.Code(appErr.Code))
	if appErr.Code != string(apperror.CodeFeatureDisabled) || spec.Status != http.StatusNotFound {
		t.Fatalf("Require() = %s/%d, want feature.disabled/404", appErr.Code, spec.Status)
	}
	if err := Require(context.Background(), flags, KeyFileUploads, Subject{TenantID: testID(1)}); err != nil {
		t.Fatalf("enabled flag returned %v", err)
	}
}

// Without an injected catalog the evaluator has no compiled definitions, so a
// nil evaluator fails closed for every flag. Callers that need defaults inject
// the composition root's catalog.
func TestRequireNilEvaluatorFailsClosed(t *testing.T) {
	for _, key := range []Key{KeyEmailVerification, KeyFileUploads} {
		if err := Require(context.Background(), nil, key, Subject{}); err == nil {
			t.Fatalf("nil evaluator must fail closed for %q", key)
		}
	}

	flags := NewEvaluator(Options{Catalog: testCatalog})
	if err := Require(context.Background(), flags, KeyEmailVerification, Subject{}); err == nil {
		t.Fatal("default-off flag must fail closed")
	}
	if err := Require(context.Background(), flags, KeyFileUploads, Subject{}); err != nil {
		t.Fatalf("default-on flag returned %v", err)
	}
}
