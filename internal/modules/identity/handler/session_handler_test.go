package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

func sessionApp(sessions *mockSessions) *fiber.App {
	h := NewSessionHandler(sessions)
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	auth := app.Group("/auth", func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 1)
		fiber.Locals[int64](c, middleware.LocalSessionID, 2)
		return c.Next()
	})
	auth.Get("/sessions", h.List)
	auth.Delete("/sessions/:id", h.Revoke)
	return app
}

func TestSessionList_MarksCurrentSession(t *testing.T) {
	app := sessionApp(&mockSessions{})
	resp, err := app.Test(httptestRequest(t, http.MethodGet, "/auth/sessions"))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var page struct {
		Data []dto.SessionResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &page))
	require.Len(t, page.Data, 2)
	assert.False(t, page.Data[0].Current)
	assert.True(t, page.Data[1].Current, "the session of the calling token is marked current")
}

func TestSessionRevoke(t *testing.T) {
	sessions := &mockSessions{}
	app := sessionApp(sessions)

	resp, err := app.Test(httptestRequest(t, http.MethodDelete, "/auth/sessions/1"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Equal(t, []int64{1}, sessions.revoked)

	resp, err = app.Test(httptestRequest(t, http.MethodDelete, "/auth/sessions/99"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode, "an unknown or foreign session is not found")
}

func httptestRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, path, http.NoBody)
	require.NoError(t, err)
	return req
}
