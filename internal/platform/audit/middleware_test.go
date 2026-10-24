package audit

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
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
	assert.Equal(t, tenant.DefaultID, recorder.events[0].TenantID,
		"requests without tenant middleware must use the compatibility tenant")
	assert.Equal(t, "POST /admin/users/:id/ban", recorder.events[0].Action)
	assert.Equal(t, "7", recorder.events[0].ResourceID)
	assert.Equal(t, "req-1", recorder.events[0].RequestID)
}

type tenantResolverFunc func(context.Context, string) (tenant.Tenant, error)

func (f tenantResolverFunc) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	return f(ctx, slug)
}

func TestMiddleware_RecordsResolvedTenant(t *testing.T) {
	recorder := &recordingRecorder{}
	resolver := tenantResolverFunc(func(_ context.Context, slug string) (tenant.Tenant, error) {
		return tenant.Tenant{ID: 2, Slug: slug, Name: "Tenant Two"}, nil
	})

	app := fiber.New()
	app.Use(tenant.Middleware(resolver, "default"))
	app.Use(Middleware(recorder))
	app.Post("/tenant/things/:id", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	req, _ := http.NewRequest("POST", "/tenant/things/9", http.NoBody)
	req.Header.Set(tenant.Header, "acme")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, int64(2), recorder.events[0].TenantID)
	assert.Equal(t, "9", recorder.events[0].ResourceID)
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

func TestMiddleware_SkipsIdempotentReplay(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Post("/mutate", func(c fiber.Ctx) error {
		fiber.Locals[bool](c, "idempotency_replayed", true)
		return c.SendStatus(fiber.StatusNoContent)
	})

	req, _ := http.NewRequest("POST", "/mutate", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Empty(t, recorder.events, "a replayed idempotent request must not be audited again")
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, counter.Write(&m))
	return m.GetCounter().GetValue()
}

func TestMiddleware_CountsAuditWriteFailures(t *testing.T) {
	before := counterValue(t, metrics.AuditWriteFailuresTotal)

	recorder := &recordingRecorder{err: errors.New("database unavailable")}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Post("/admin/users/:id/ban", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	req, _ := http.NewRequest("POST", "/admin/users/7/ban", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)

	assert.Equal(t, before+1, counterValue(t, metrics.AuditWriteFailuresTotal),
		"a failed audit write must increment audit_write_failures_total")
}
