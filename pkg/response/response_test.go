package response

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func newTestApp(handler fiber.Handler) *fiber.App {
	app := fiber.New()
	app.Get("/test", handler)
	return app
}

func doRequest(t *testing.T, app *fiber.App) (resp *http.Response, parsed map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) > 0 {
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}
	}
	return resp, parsed
}

func TestSuccess(t *testing.T) {
	app := newTestApp(func(c fiber.Ctx) error {
		return Success(c, map[string]string{"key": "value"})
	})

	resp, result := doRequest(t, app)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if result["success"] != true {
		t.Error("success should be true")
	}
	data, ok := result["data"].(map[string]any)
	if !ok {
		t.Fatal("data should be a map")
	}
	if data["key"] != "value" {
		t.Errorf("data.key = %v, want value", data["key"])
	}
}

func TestCreated(t *testing.T) {
	app := fiber.New()
	app.Post("/test", func(c fiber.Ctx) error {
		return Created(c, map[string]string{"id": "1"})
	})

	req, _ := http.NewRequest("POST", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if result["success"] != true {
		t.Error("success should be true")
	}
}

func TestNoContent(t *testing.T) {
	app := newTestApp(NoContent)

	resp, _ := doRequest(t, app)
	if resp.StatusCode != 204 {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestSuccessWithMeta(t *testing.T) {
	app := newTestApp(func(c fiber.Ctx) error {
		meta := NewMeta(1, 10, 25)
		return SuccessWithMeta(c, []string{"a", "b"}, meta)
	})

	resp, result := doRequest(t, app)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if result["success"] != true {
		t.Error("success should be true")
	}
	meta, ok := result["meta"].(map[string]any)
	if !ok {
		t.Fatal("meta should be a map")
	}
	if meta["page"] != float64(1) {
		t.Errorf("meta.page = %v, want 1", meta["page"])
	}
	if meta["per_page"] != float64(10) {
		t.Errorf("meta.per_page = %v, want 10", meta["per_page"])
	}
	if meta["total"] != float64(25) {
		t.Errorf("meta.total = %v, want 25", meta["total"])
	}
	if meta["total_page"] != float64(3) {
		t.Errorf("meta.total_page = %v, want 3", meta["total_page"])
	}
}

func TestError(t *testing.T) {
	app := newTestApp(func(c fiber.Ctx) error {
		return Error(c, 400, "BAD_REQUEST", "invalid input")
	})

	resp, result := doRequest(t, app)
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if result["success"] != false {
		t.Error("success should be false")
	}
	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatal("error should be a map")
	}
	if errObj["code"] != "BAD_REQUEST" {
		t.Errorf("error.code = %v, want BAD_REQUEST", errObj["code"])
	}
	if errObj["message"] != "invalid input" {
		t.Errorf("error.message = %v, want invalid input", errObj["message"])
	}
}

func TestErrorWithDetails(t *testing.T) {
	app := newTestApp(func(c fiber.Ctx) error {
		return ErrorWithDetails(c, 422, "VALIDATION_ERROR", "validation failed",
			map[string]string{"field": "required"})
	})

	resp, result := doRequest(t, app)
	if resp.StatusCode != 422 {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	errObj := result["error"].(map[string]any)
	if errObj["details"] == nil {
		t.Error("details should not be nil")
	}
}

func TestNewMeta(t *testing.T) {
	meta := NewMeta(2, 10, 25)
	if meta.Page != 2 {
		t.Errorf("Page = %d, want 2", meta.Page)
	}
	if meta.PerPage != 10 {
		t.Errorf("PerPage = %d, want 10", meta.PerPage)
	}
	if meta.Total != 25 {
		t.Errorf("Total = %d, want 25", meta.Total)
	}
	if meta.TotalPage != 3 {
		t.Errorf("TotalPage = %d, want 3", meta.TotalPage)
	}
}
