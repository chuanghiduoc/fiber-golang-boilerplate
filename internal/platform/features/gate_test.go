package features

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
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

func TestMiddlewareEvaluatesRequestTenantAndUser(t *testing.T) {
	snapshot := Snapshot{
		UserTargets: map[Key]map[UserTargetKey]bool{
			KeyFileUploads: {{TenantID: 7, UserID: 9}: false},
		},
	}
	flags := NewEvaluator(Options{Store: &fakeStore{snapshot: snapshot}})

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), 7))
		if c.Get("X-User") == "9" {
			fiber.Locals[int64](c, "user_id", 9)
		}
		return c.Next()
	})
	app.Post("/upload", Middleware(flags, KeyFileUploads), func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusCreated)
	})

	req := httptest.NewRequest(http.MethodPost, "/upload", http.NoBody)
	req.Header.Set("X-User", "9")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("targeted user status = %d, want 404", resp.StatusCode)
	}

	resp, err = app.Test(httptest.NewRequest(http.MethodPost, "/upload", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("other user status = %d, want 201", resp.StatusCode)
	}
}
