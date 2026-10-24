package middleware

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestIsMultipartForm(t *testing.T) {
	for value, want := range map[string]bool{
		"multipart/form-data; boundary=x":   true,
		"Multipart/Form-Data; boundary=x":   true,
		" multipart/form-data ":             true,
		"application/json":                  false,
		"multipart/mixed; boundary=x":       false,
		"application/x-www-form-urlencoded": false,
		"":                                  false,
	} {
		if got := isMultipartForm(value); got != want {
			t.Errorf("isMultipartForm(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestBodyLimit(t *testing.T) {
	const limit = 16
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler, BodyLimit: 1 << 20})
	app.Use(BodyLimit(limit))
	app.Post("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	tests := []struct {
		name        string
		contentType string
		body        string
		chunked     bool
		want        int
	}{
		{"small JSON", fiber.MIMEApplicationJSON, `{"a":1}`, false, fiber.StatusOK},
		{"JSON at limit", fiber.MIMEApplicationJSON, strings.Repeat("a", limit), false, fiber.StatusOK},
		{"JSON over limit", fiber.MIMEApplicationJSON, strings.Repeat("a", limit+1), false, fiber.StatusRequestEntityTooLarge},
		{"no content type over limit", "", strings.Repeat("a", limit+1), false, fiber.StatusRequestEntityTooLarge},
		{"chunked JSON over limit", fiber.MIMEApplicationJSON, strings.Repeat("a", limit+1), true, fiber.StatusRequestEntityTooLarge},
		{"multipart keeps the upload limit", "multipart/form-data", strings.Repeat("a", limit*4), false, fiber.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := tt.body, tt.contentType
			if strings.HasPrefix(strings.ToLower(contentType), "multipart/") {
				body, contentType = multipartBody(t, "file", tt.body)
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if contentType != "" {
				req.Header.Set(fiber.HeaderContentType, contentType)
			}
			if tt.chunked {
				// No Content-Length: the size is known only from the body.
				req.ContentLength = 0
				req.TransferEncoding = []string{"chunked"}
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want != fiber.StatusRequestEntityTooLarge {
				return
			}
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if problem.Code != "payload_too_large" {
				t.Errorf("code = %q, want payload_too_large", problem.Code)
			}
		})
	}
}

func TestBodyLimit_RejectsCompressedBodies(t *testing.T) {
	const limit = 1024
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler, BodyLimit: 1 << 20})
	app.Use(BodyLimit(limit))
	app.Post("/", func(c fiber.Ctx) error {
		// c.Body() would transparently decompress; the middleware must stop
		// the request before any handler can bind an expanded body.
		return c.SendString(strconv.Itoa(len(c.Body())))
	})

	// A gzip body far below the limit that expands to many times the limit.
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"a":"` + strings.Repeat("a", limit*64) + `"}`)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if compressed.Len() >= limit {
		t.Fatalf("compressed body is %d bytes; the test needs it below the %d limit", compressed.Len(), limit)
	}

	tests := []struct {
		name     string
		encoding string
		body     []byte
		multi    bool
		want     int
	}{
		{"gzip JSON", "gzip", compressed.Bytes(), false, fiber.StatusUnsupportedMediaType},
		{"uppercase br", "BR", []byte("x"), false, fiber.StatusUnsupportedMediaType},
		{"deflate", "deflate", []byte("x"), false, fiber.StatusUnsupportedMediaType},
		{"zstd", "zstd", []byte("x"), false, fiber.StatusUnsupportedMediaType},
		{"identity then gzip", "identity, gzip", compressed.Bytes(), false, fiber.StatusUnsupportedMediaType},
		{"gzip multipart", "gzip", nil, true, fiber.StatusUnsupportedMediaType},
		{"identity", "identity", []byte(`{"a":1}`), false, fiber.StatusOK},
		{"no encoding", "", []byte(`{"a":1}`), false, fiber.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := string(tt.body), fiber.MIMEApplicationJSON
			if tt.multi {
				body, contentType = multipartBody(t, "file", "data")
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set(fiber.HeaderContentType, contentType)
			if tt.encoding != "" {
				req.Header.Set(fiber.HeaderContentEncoding, tt.encoding)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want != fiber.StatusUnsupportedMediaType {
				return
			}
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if problem.Code != "unsupported_media_type" {
				t.Errorf("code = %q, want unsupported_media_type", problem.Code)
			}
		})
	}
}
