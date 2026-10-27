package audithttp

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

type recordingRecorder struct {
	events []audit.Event
	err    error
}

func (r *recordingRecorder) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return r.err
}

func TestMiddleware_RecordsSuccessfulMutation(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(requestctx.WithActorID(c.Context(), testID(42)))
		c.SetContext(requestctx.WithRequestID(c.Context(), "req-1"))
		c.SetContext(tenant.WithID(c.Context(), tenant.DefaultID))
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
	assert.Equal(t, testID(42), recorder.events[0].ActorID)
	assert.Equal(t, tenant.DefaultID, recorder.events[0].TenantID,
		"the request tenant is recorded from the explicit request context")
	assert.Equal(t, "POST /admin/users/:id/ban", recorder.events[0].Action)
	assert.Equal(t, "7", recorder.events[0].ResourceID)
	assert.Equal(t, "req-1", recorder.events[0].RequestID)
}

type tenantResolverFunc func(context.Context, string) (tenant.Tenant, error)

func (f tenantResolverFunc) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	return f(ctx, slug)
}

func TestMiddleware_MergesHandlerMetadata(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Post("/admin/api-keys", func(c fiber.Ctx) error {
		fiber.Locals[map[string]any](c, audit.MetadataLocalKey, map[string]any{
			"apiKeyId":   int64(5),
			"apiKeyName": "deploy",
			"scopes":     []string{"files:read"},
		})
		return c.SendStatus(fiber.StatusCreated)
	})

	req, _ := http.NewRequest("POST", "/admin/api-keys", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
	require.Len(t, recorder.events, 1)
	assert.Equal(t, int64(5), recorder.events[0].Metadata["apiKeyId"])
	assert.Equal(t, "deploy", recorder.events[0].Metadata["apiKeyName"])
	assert.Equal(t, []string{"files:read"}, recorder.events[0].Metadata["scopes"])
	assert.Equal(t, fiber.StatusCreated, recorder.events[0].Metadata["status"],
		"generic metadata must survive the merge")
}

func TestMiddleware_RecordsResolvedTenant(t *testing.T) {
	recorder := &recordingRecorder{}
	resolver := tenantResolverFunc(func(_ context.Context, slug string) (tenant.Tenant, error) {
		return tenant.Tenant{ID: testID(2), Slug: slug, Name: "Tenant Two"}, nil
	})

	app := fiber.New()
	app.Use(tenanthttp.Middleware(resolver, "default"))
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
	assert.Equal(t, testID(2), recorder.events[0].TenantID)
	assert.Equal(t, "9", recorder.events[0].ResourceID)
}

func TestMiddleware_MergesDomainMetadata(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Put("/admin/features/flags/:key", func(c fiber.Ctx) error {
		AddMetadata(c, map[string]any{
			"feature": "file_uploads",
			"enabled": true,
		})
		AddMetadata(c, map[string]any{"killSwitch": false})
		return c.SendStatus(fiber.StatusOK)
	})

	req, _ := http.NewRequest("PUT", "/admin/features/flags/file_uploads", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Len(t, recorder.events, 1)
	event := recorder.events[0]
	assert.Equal(t, "file_uploads", event.ResourceID, "key path params must populate the audit resource id")
	assert.Equal(t, "file_uploads", event.Metadata["feature"])
	assert.Equal(t, true, event.Metadata["enabled"])
	assert.Equal(t, false, event.Metadata["killSwitch"])
	assert.Equal(t, fiber.StatusOK, event.Metadata["status"])
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

func TestMiddleware_SkipsGuardedRoute(t *testing.T) {
	recorder := &recordingRecorder{}
	app := fiber.New()
	app.Use(Middleware(recorder))
	app.Post("/guarded", func(c fiber.Ctx) error {
		fiber.Locals[bool](c, "audit_guarded", true)
		return c.SendStatus(fiber.StatusNoContent)
	})

	req, _ := http.NewRequest("POST", "/guarded", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
	assert.Empty(t, recorder.events, "a Guard-owned route must not also use the global fallback")
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

func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	value, _ := metrics.SnapshotValue(name)
	return value
}

func TestMiddleware_CountsAuditWriteFailures(t *testing.T) {
	before := counterValue(t, "audit_write_failures_total")

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

	assert.Equal(t, before+1, counterValue(t, "audit_write_failures_total"),
		"a failed audit write must increment audit_write_failures_total")
}
