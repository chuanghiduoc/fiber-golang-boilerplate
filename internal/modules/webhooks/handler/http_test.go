package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

const testRequestID = "webhooks-handler-test"

// recordingAudit satisfies the required-Guard recorder contract without a
// database: intent rows and completions succeed.
type recordingAudit struct{}

func (recordingAudit) Record(context.Context, audit.Event) error { return nil }

func (recordingAudit) RecordID(context.Context, audit.Event) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (recordingAudit) Complete(context.Context, uuid.UUID, audit.Completion) error { return nil }

var testActorID = uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001")

// fakeService implements the webhooks service surface with per-test functions.
// A method with no installed function fails loudly instead of silently
// returning zero data.
type fakeService struct {
	webhookservice.Service

	createEndpoint       func(context.Context, uuid.UUID, webhookservice.CreateEndpointInput) (*webhookservice.Endpoint, string, error)
	listEndpoints        func(context.Context, int, string) ([]webhookservice.Endpoint, bool, error)
	getEndpoint          func(context.Context, uuid.UUID) (*webhookservice.Endpoint, error)
	updateEndpoint       func(context.Context, uuid.UUID, uuid.UUID, webhookservice.UpdateEndpointInput) (*webhookservice.Endpoint, error)
	deleteEndpoint       func(context.Context, uuid.UUID, uuid.UUID) error
	rotateEndpointSecret func(context.Context, uuid.UUID, uuid.UUID, bool) (*webhookservice.Endpoint, string, error)
	listDeliveries       func(context.Context, uuid.UUID, string, int, string) ([]webhookservice.Delivery, bool, error)
	getDelivery          func(context.Context, uuid.UUID) (*webhookservice.Delivery, error)
	listDeliveryAttempts func(context.Context, uuid.UUID) ([]webhookservice.DeliveryAttempt, error)
	replayDelivery       func(context.Context, uuid.UUID, uuid.UUID) (*webhookservice.Delivery, error)
	createInboundSource  func(context.Context, uuid.UUID, webhookservice.CreateInboundSourceInput) (*webhookservice.InboundSource, string, error)
	listInboundSources   func(context.Context, int, string) ([]webhookservice.InboundSource, bool, error)
	getInboundSource     func(context.Context, uuid.UUID) (*webhookservice.InboundSource, error)
	updateInboundSource  func(context.Context, uuid.UUID, uuid.UUID, webhookservice.UpdateInboundSourceInput) (*webhookservice.InboundSource, error)
	deleteInboundSource  func(context.Context, uuid.UUID, uuid.UUID) error
	rotateInboundSecret  func(context.Context, uuid.UUID, uuid.UUID, bool) (*webhookservice.InboundSource, string, error)
	listInboundEvents    func(context.Context, uuid.UUID, int, string) ([]webhookservice.InboundEvent, bool, error)
	receiveInbound       func(context.Context, uuid.UUID, http.Header, []byte) (*webhookservice.InboundReceipt, error)
}

func missing(method string) {
	panic("fakeService: " + method + " is not installed by the test")
}

func (f *fakeService) CreateEndpoint(ctx context.Context, actorID uuid.UUID, in webhookservice.CreateEndpointInput) (*webhookservice.Endpoint, string, error) {
	if f.createEndpoint == nil {
		missing("CreateEndpoint")
	}
	return f.createEndpoint(ctx, actorID, in)
}

func (f *fakeService) ListEndpoints(ctx context.Context, limit int, startingAfter string) ([]webhookservice.Endpoint, bool, error) {
	if f.listEndpoints == nil {
		missing("ListEndpoints")
	}
	return f.listEndpoints(ctx, limit, startingAfter)
}

func (f *fakeService) GetEndpoint(ctx context.Context, id uuid.UUID) (*webhookservice.Endpoint, error) {
	if f.getEndpoint == nil {
		missing("GetEndpoint")
	}
	return f.getEndpoint(ctx, id)
}

func (f *fakeService) UpdateEndpoint(ctx context.Context, actorID, id uuid.UUID, in webhookservice.UpdateEndpointInput) (*webhookservice.Endpoint, error) {
	if f.updateEndpoint == nil {
		missing("UpdateEndpoint")
	}
	return f.updateEndpoint(ctx, actorID, id, in)
}

func (f *fakeService) DeleteEndpoint(ctx context.Context, actorID, id uuid.UUID) error {
	if f.deleteEndpoint == nil {
		missing("DeleteEndpoint")
	}
	return f.deleteEndpoint(ctx, actorID, id)
}

func (f *fakeService) RotateEndpointSecret(ctx context.Context, actorID, id uuid.UUID, expirePrevious bool) (*webhookservice.Endpoint, string, error) {
	if f.rotateEndpointSecret == nil {
		missing("RotateEndpointSecret")
	}
	return f.rotateEndpointSecret(ctx, actorID, id, expirePrevious)
}

func (f *fakeService) ListDeliveries(ctx context.Context, endpointID uuid.UUID, status string, limit int, startingAfter string) ([]webhookservice.Delivery, bool, error) {
	if f.listDeliveries == nil {
		missing("ListDeliveries")
	}
	return f.listDeliveries(ctx, endpointID, status, limit, startingAfter)
}

func (f *fakeService) GetDelivery(ctx context.Context, id uuid.UUID) (*webhookservice.Delivery, error) {
	if f.getDelivery == nil {
		missing("GetDelivery")
	}
	return f.getDelivery(ctx, id)
}

func (f *fakeService) ListDeliveryAttempts(ctx context.Context, id uuid.UUID) ([]webhookservice.DeliveryAttempt, error) {
	if f.listDeliveryAttempts == nil {
		missing("ListDeliveryAttempts")
	}
	return f.listDeliveryAttempts(ctx, id)
}

func (f *fakeService) ReplayDelivery(ctx context.Context, actorID, id uuid.UUID) (*webhookservice.Delivery, error) {
	if f.replayDelivery == nil {
		missing("ReplayDelivery")
	}
	return f.replayDelivery(ctx, actorID, id)
}

func (f *fakeService) CreateInboundSource(ctx context.Context, actorID uuid.UUID, in webhookservice.CreateInboundSourceInput) (*webhookservice.InboundSource, string, error) {
	if f.createInboundSource == nil {
		missing("CreateInboundSource")
	}
	return f.createInboundSource(ctx, actorID, in)
}

func (f *fakeService) ListInboundSources(ctx context.Context, limit int, startingAfter string) ([]webhookservice.InboundSource, bool, error) {
	if f.listInboundSources == nil {
		missing("ListInboundSources")
	}
	return f.listInboundSources(ctx, limit, startingAfter)
}

func (f *fakeService) GetInboundSource(ctx context.Context, id uuid.UUID) (*webhookservice.InboundSource, error) {
	if f.getInboundSource == nil {
		missing("GetInboundSource")
	}
	return f.getInboundSource(ctx, id)
}

func (f *fakeService) UpdateInboundSource(ctx context.Context, actorID, id uuid.UUID, in webhookservice.UpdateInboundSourceInput) (*webhookservice.InboundSource, error) {
	if f.updateInboundSource == nil {
		missing("UpdateInboundSource")
	}
	return f.updateInboundSource(ctx, actorID, id, in)
}

func (f *fakeService) DeleteInboundSource(ctx context.Context, actorID, id uuid.UUID) error {
	if f.deleteInboundSource == nil {
		missing("DeleteInboundSource")
	}
	return f.deleteInboundSource(ctx, actorID, id)
}

func (f *fakeService) RotateInboundSourceSecret(ctx context.Context, actorID, id uuid.UUID, expirePrevious bool) (*webhookservice.InboundSource, string, error) {
	if f.rotateInboundSecret == nil {
		missing("RotateInboundSourceSecret")
	}
	return f.rotateInboundSecret(ctx, actorID, id, expirePrevious)
}

func (f *fakeService) ListInboundEvents(ctx context.Context, sourceID uuid.UUID, limit int, startingAfter string) ([]webhookservice.InboundEvent, bool, error) {
	if f.listInboundEvents == nil {
		missing("ListInboundEvents")
	}
	return f.listInboundEvents(ctx, sourceID, limit, startingAfter)
}

func (f *fakeService) ReceiveInbound(ctx context.Context, sourceID uuid.UUID, headers http.Header, body []byte) (*webhookservice.InboundReceipt, error) {
	if f.receiveInbound == nil {
		missing("ReceiveInbound")
	}
	return f.receiveInbound(ctx, sourceID, headers, body)
}

// newAPI builds the HTTP surface the way internal/app does: one huma API over
// Fiber with the policy chain, and the module's RouteProvider registration.
func newAPI(t *testing.T, svc webhookservice.Service) (*fiber.App, *httpapi.API) {
	t.Helper()
	pass := func(c fiber.Ctx) error { return c.Next() }
	authenticate := func(c fiber.Ctx) error {
		authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(
			testActorID, tenant.DefaultID, authz.RoleAdmin,
			"webhooks:read", "webhooks:write",
		))
		return c.Next()
	}

	fiberApp := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	fiberApp.Use(func(c fiber.Ctx) error {
		fiber.Locals[string](c, "request_id", testRequestID)
		ctx := requestctx.WithRequestID(c.Context(), testRequestID)
		c.SetContext(requestctx.WithPath(ctx, c.Path()))
		return c.Next()
	})
	api := httpapi.New(fiberApp, fiberApp, httpapi.DefaultConfig("Webhooks", "1.0.0", "test"), httpapi.Deps{
		Limiters:        httpapi.Limiters{Strict: pass, Normal: pass, Relaxed: pass},
		Authenticate:    authenticate,
		AuthenticateJWT: authenticate,
		Idempotency:     pass,
		Audit:           recordingAudit{},
		MFAMaxAge:       time.Hour,
	})
	webhooks.New(webhookshandler.New(svc)).RegisterRoutes(api, app.Deps{})
	return fiberApp, api
}

// do sends one request through the Fiber test adapter.
func do(t *testing.T, fiberApp *fiber.App, method, path string, body any, headers map[string]string) (resp *http.Response, responseBody []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := fiberApp.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, data
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(body, &out), "body: %s", body)
	return out
}

func endpointFixture(id uuid.UUID, updatedAt time.Time) *webhookservice.Endpoint {
	return &webhookservice.Endpoint{
		ID:          id,
		TenantID:    tenant.DefaultID,
		URL:         "https://receiver.example/hook",
		Description: "orders",
		EventTypes:  []string{"file.uploaded"},
		Status:      webhookservice.StatusActive,
		MaxAttempts: 8,
		TimeoutMS:   10000,
		CreatedAt:   updatedAt,
		UpdatedAt:   updatedAt,
	}
}

func sourceFixture(id uuid.UUID, updatedAt time.Time) *webhookservice.InboundSource {
	return &webhookservice.InboundSource{
		ID:        id,
		TenantID:  tenant.DefaultID,
		Name:      "Stripe",
		Slug:      "stripe",
		Status:    webhookservice.StatusActive,
		CreatedAt: updatedAt,
		UpdatedAt: updatedAt,
	}
}

// TestInvalidUUIDIdentifiersAreRejected proves a non-UUID path id or filter
// never reaches the service.
func TestInvalidUUIDIdentifiersAreRejected(t *testing.T) {
	svc := &fakeService{}
	fiberApp, _ := newAPI(t, svc)

	for _, tc := range []struct {
		name    string
		method  string
		path    string
		body    any
		headers map[string]string
	}{
		{"endpoint path", http.MethodGet, "/api/v1/webhooks/endpoints/not-a-uuid", nil, nil},
		{"delivery path", http.MethodGet, "/api/v1/webhooks/deliveries/not-a-uuid", nil, nil},
		{"endpoint filter", http.MethodGet, "/api/v1/webhooks/deliveries?endpointId=not-a-uuid", nil, nil},
		{"source path", http.MethodGet, "/api/v1/webhooks/inbound-sources/not-a-uuid", nil, nil},
		{"receiver path", http.MethodPost, "/api/v1/webhooks/inbound/not-a-uuid", map[string]any{"type": "payment.succeeded"}, map[string]string{
			"webhook-id": "msg_1", "webhook-timestamp": "1700000000", "webhook-signature": "v1,abc",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, fiberApp, tc.method, tc.path, tc.body, tc.headers)
			require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, "body: %s", body)
			problem := decode[apperror.ProblemDetails](t, body)
			assert.Equal(t, string(apperror.CodeRequestValidationFailed), problem.Code)
		})
	}
}
