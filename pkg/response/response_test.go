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
	// Success returns the resource directly, with no envelope.
	if result["key"] != "value" {
		t.Errorf("body.key = %v, want value", result["key"])
	}
	if _, hasEnvelope := result["data"]; hasEnvelope {
		t.Error("success response should not wrap data in an envelope")
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
	if result["id"] != "1" {
		t.Errorf("body.id = %v, want 1", result["id"])
	}
}

func TestNoContent(t *testing.T) {
	app := newTestApp(NoContent)

	resp, _ := doRequest(t, app)
	if resp.StatusCode != 204 {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

func TestList(t *testing.T) {
	app := newTestApp(func(c fiber.Ctx) error {
		return List(c, []string{"a", "b"}, true)
	})

	resp, result := doRequest(t, app)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if result["object"] != "list" {
		t.Errorf("object = %v, want list", result["object"])
	}
	if result["url"] != "/test" {
		t.Errorf("url = %v, want /test", result["url"])
	}
	if result["hasMore"] != true {
		t.Errorf("hasMore = %v, want true", result["hasMore"])
	}
	data, ok := result["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("data should be a 2-element array, got %v", result["data"])
	}
}

func TestList_EmptySerialisesAsArray(t *testing.T) {
	app := newTestApp(func(c fiber.Ctx) error {
		return List(c, []string{}, false)
	})

	_, result := doRequest(t, app)
	data, ok := result["data"].([]any)
	if !ok {
		t.Fatalf("empty data should serialise as [], got %T", result["data"])
	}
	if len(data) != 0 {
		t.Errorf("expected empty array, got %v", data)
	}
}
