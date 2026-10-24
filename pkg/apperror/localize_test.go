package apperror

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type mapLocalizer map[string]string

func (m mapLocalizer) Localize(_ fiber.Ctx, key string, params map[string]any) string {
	message, ok := m[key]
	if !ok {
		return ""
	}
	for name, value := range params {
		message = strings.ReplaceAll(message, "{"+name+"}", fmt.Sprint(value))
	}
	return message
}

func decodeWithHandler(t *testing.T, handler fiber.ErrorHandler, route fiber.Handler) (body map[string]any, header http.Header, status int) {
	t.Helper()
	app := fiber.New(fiber.Config{ErrorHandler: handler})
	app.Get("/x", route)
	req, err := http.NewRequest(http.MethodGet, "/x", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return body, resp.Header, resp.StatusCode
}

func TestLocalizedTitleAndDetail(t *testing.T) {
	localizer := mapLocalizer{
		"errors.user.not_found.title": "Không tìm thấy người dùng",
		"detail.user.not_found":       "Không tìm thấy người dùng {id}",
	}
	body, _, status := decodeWithHandler(t,
		FiberErrorHandlerWithLocalizer(localizer),
		func(c fiber.Ctx) error {
			return NewCode(CodeUserNotFound, "user missing").WithDetailKey("detail.user.not_found", map[string]any{"id": 7})
		},
	)

	if status != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if body["title"] != "Không tìm thấy người dùng" {
		t.Errorf("title = %v, want localized title", body["title"])
	}
	if body["detail"] != "Không tìm thấy người dùng 7" {
		t.Errorf("detail = %v, want localized detail", body["detail"])
	}
	// The machine-readable contract must not change.
	if body["code"] != string(CodeUserNotFound) {
		t.Errorf("code = %v, want %s", body["code"], CodeUserNotFound)
	}
	if body["type"] != "/errors/user-not-found" {
		t.Errorf("type = %v, want /errors/user-not-found", body["type"])
	}
	if body["instance"] != "/x" {
		t.Errorf("instance = %v, want /x", body["instance"])
	}
	if body["requestId"] == nil {
		t.Error("requestId should be present")
	}
}

func TestLocalizedValidationMessages(t *testing.T) {
	localizer := mapLocalizer{
		"errors.validation_failed.title": "Dữ liệu không hợp lệ",
		"validation.required":            "Trường {path} là bắt buộc.",
		"validation.too_short":           "Trường {path} phải có ít nhất {min} ký tự.",
		"validation.too_long":            "Trường {path} phải có tối đa {max} ký tự.",
	}
	body, _, status := decodeWithHandler(t,
		FiberErrorHandlerWithLocalizer(localizer),
		func(c fiber.Ctx) error {
			return NewValidation("fallback detail", []FieldError{
				{Path: "name", Code: "required", Message: "name is required"},
				{Path: "bio", Code: "too_short", Message: "bio is too short", Params: map[string]string{"min": "3"}},
				{Path: "code", Code: "too_long", Message: "code is too long", Params: map[string]string{"max": "5"}},
			})
		},
	)

	if status != fiber.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", status)
	}
	if body["title"] != "Dữ liệu không hợp lệ" {
		t.Errorf("title = %v, want localized", body["title"])
	}
	if body["detail"] != "fallback detail" {
		t.Errorf("detail = %v, literal detail should be preserved without DetailKey", body["detail"])
	}
	fields, ok := body["errors"].([]any)
	if !ok || len(fields) != 3 {
		t.Fatalf("errors[] = %v", body["errors"])
	}
	first := fields[0].(map[string]any)
	if first["path"] != "name" || first["code"] != "required" {
		t.Errorf("field identity changed: %v", first)
	}
	if first["message"] != "Trường name là bắt buộc." {
		t.Errorf("required message = %v", first["message"])
	}
	if fields[1].(map[string]any)["message"] != "Trường bio phải có ít nhất 3 ký tự." {
		t.Errorf("min message = %v", fields[1])
	}
	if fields[2].(map[string]any)["message"] != "Trường code phải có tối đa 5 ký tự." {
		t.Errorf("max message = %v", fields[2])
	}
	if _, exists := first["params"]; exists {
		t.Error("validation params must not leak into the wire contract")
	}
}

func TestLocalizerFallbackKeepsEnglish(t *testing.T) {
	body, _, _ := decodeWithHandler(t,
		FiberErrorHandlerWithLocalizer(mapLocalizer{}),
		func(c fiber.Ctx) error {
			return NewCode(CodeUserNotFound, "user missing").WithDetailKey("detail.unknown", nil)
		},
	)
	if body["title"] != "User not found" {
		t.Errorf("title = %v, want English fallback", body["title"])
	}
	if body["detail"] != "user missing" {
		t.Errorf("detail = %v, want literal fallback", body["detail"])
	}
}

func TestNilLocalizerPreservesCurrentBehavior(t *testing.T) {
	for name, handler := range map[string]fiber.ErrorHandler{
		"plain":       FiberErrorHandler,
		"nil wrapped": FiberErrorHandlerWithLocalizer(nil),
	} {
		t.Run(name, func(t *testing.T) {
			body, _, _ := decodeWithHandler(t, handler, func(c fiber.Ctx) error {
				return NewValidation("bad fields", []FieldError{
					{Path: "name", Code: "required", Message: "name is required"},
				})
			})
			if body["title"] != "Validation failed" {
				t.Errorf("title = %v, want Validation failed", body["title"])
			}
			if body["detail"] != "bad fields" {
				t.Errorf("detail = %v, want bad fields", body["detail"])
			}
			fields := body["errors"].([]any)
			if fields[0].(map[string]any)["message"] != "name is required" {
				t.Errorf("message = %v, want original", fields[0])
			}
		})
	}
}
