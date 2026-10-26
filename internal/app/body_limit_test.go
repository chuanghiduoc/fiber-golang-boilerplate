package app_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
)

// TestOversizedJSONRejectedWithoutReadingIt proves the global JSON body limit
// runs while the request body is still streaming: the server answers from the
// Content-Length header after reading only its small pre-read buffer, without
// waiting for the remaining 2 MiB. Fiber's non-streaming mode buffers the
// whole body first, so this test times out against that configuration.
func TestOversizedJSONRejectedWithoutReadingIt(t *testing.T) {
	cfg := &config.Config{
		App: config.AppConfig{
			Env:            "production",
			BodyLimit:      12 << 20,
			JSONBodyLimit:  1 << 20,
			RequestTimeout: 5,
		},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(pingModule{})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- application.Fiber().Listener(listener) }()
	t.Cleanup(func() {
		_ = application.Fiber().Shutdown()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
		}
	})

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	const bodySize = 2 << 20
	_, err = fmt.Fprintf(conn,
		"POST /api/v1/ping HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		bodySize)
	require.NoError(t, err)
	// Send only the bytes fasthttp prefetches before it calls the handler. The
	// rest of the declared body is never written.
	_, err = conn.Write(bytes.Repeat([]byte("a"), 8<<10))
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	require.NoError(t, err, "the server must reject from Content-Length without waiting for the body")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

// uploadPathModule stands in for the files module at the one path registered
// with the larger multipart limit.
type uploadPathModule struct{}

func (uploadPathModule) Name() string { return "upload" }
func (uploadPathModule) RegisterRoutes(v1 fiber.Router, _ app.Deps) {
	v1.Post("/files/upload", func(c fiber.Ctx) error {
		header, err := c.FormFile("file")
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return c.SendString(fmt.Sprintf("%d", header.Size))
	})
}

// TestUploadPathAcceptsMultipartAboveJSONLimit proves the upload route keeps
// APP_BODY_LIMIT for multipart after the global cap dropped to the JSON limit,
// and that lazy multipart parsing sees the whole file while streaming.
func TestUploadPathAcceptsMultipartAboveJSONLimit(t *testing.T) {
	cfg := &config.Config{
		App: config.AppConfig{
			Env:            "production",
			BodyLimit:      12 << 20,
			JSONBodyLimit:  1 << 20,
			RequestTimeout: 5,
		},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{})
	application.Register(uploadPathModule{})

	const fileSize = 2 << 20
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "large.txt")
	require.NoError(t, err)
	_, err = part.Write(bytes.Repeat([]byte("x"), fileSize))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req, err := http.NewRequest(http.MethodPost, "/api/v1/files/upload", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := application.Fiber().Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	assert.Equal(t, fmt.Sprintf("%d", fileSize), string(got))
}
