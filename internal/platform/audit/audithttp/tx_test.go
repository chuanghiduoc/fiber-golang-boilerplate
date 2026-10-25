package audithttp

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

// sharedTxRecorder lets one recorder serve both the fallback middleware
// (Record) and a Tier 1 service (RecordInTx), as in cmd/api.
type sharedTxRecorder struct{ recordingRecorder }

func (r *sharedTxRecorder) RecordTx(ctx context.Context, _ pgx.Tx, event audit.Event) error {
	return r.Record(ctx, event)
}

func TestMiddleware_SkipsRequestsAuditedByTier1Service(t *testing.T) {
	recorder := &sharedTxRecorder{}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Post("/auth/register", func(c fiber.Ctx) error {
		event := audit.NewServiceEvent(c.Context(), "auth.registered", "user")
		if err := audit.RecordInTx(c.Context(), recorder, nil, event); err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusCreated)
	})
	app.Post("/plain", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusCreated)
	})

	resp, err := app.Test(mustRequest(t, "/auth/register"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
	require.Len(t, recorder.events, 1, "Tier 1 route must produce exactly one audit row")
	assert.Equal(t, "auth.registered", recorder.events[0].Action)

	// Routes without a service event still get the generic fallback row.
	resp, err = app.Test(mustRequest(t, "/plain"))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
	require.Len(t, recorder.events, 2)
	assert.Equal(t, "POST /plain", recorder.events[1].Action)
}

func mustRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, path, http.NoBody)
	require.NoError(t, err)
	return req
}
