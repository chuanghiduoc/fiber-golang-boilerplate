package audit

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRecorder struct {
	events []Event
	err    error
}

func (r *recordingRecorder) Record(_ context.Context, event Event) error {
	r.events = append(r.events, event)
	return r.err
}

func TestMiddleware_RecordsSuccessfulMutation(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 42)
		fiber.Locals[string](c, "request_id", "req-1")
		return c.Next()
	})
	app.Use(Middleware(recorder))
	app.Post("/admin/users/:id/ban", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	req, _ := http.NewRequest("POST", "/admin/users/7/ban", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, int64(42), recorder.events[0].ActorID)
	assert.Equal(t, "POST /admin/users/:id/ban", recorder.events[0].Action)
	assert.Equal(t, "7", recorder.events[0].ResourceID)
	assert.Equal(t, "req-1", recorder.events[0].RequestID)
}

func TestMiddleware_SkipsReadsAndFailures(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Get("/things", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	app.Post("/fail", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusBadRequest, "bad")
	})

	req, _ := http.NewRequest("GET", "/things", http.NoBody)
	_, _ = app.Test(req)
	req, _ = http.NewRequest("POST", "/fail", http.NoBody)
	_, _ = app.Test(req)

	assert.Empty(t, recorder.events)
}
