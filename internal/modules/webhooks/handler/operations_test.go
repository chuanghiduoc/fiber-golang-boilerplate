package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// TestOperationsMatchTheCanonicalRouteMap pins method, path, operationId, and
// policy for every webhooks operation in docs/api-routes.md.
func TestOperationsMatchTheCanonicalRouteMap(t *testing.T) {
	_, api := newAPI(t, &fakeService{})
	const base = "/api/v1/webhooks"
	type want struct {
		method, path, operationID string
		public                    bool
		permission                authz.Permission
		rate                      httpapi.RateLimitTier
		idempotent, noStore       bool
		tier1                     string
		guardAction, guardRes     string
	}
	expected := []want{
		{"GET", base + "/endpoints", "listWebhookEndpoints", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"POST", base + "/endpoints", "createWebhookEndpoint", false, "webhooks:write", httpapi.RateLimitNormal, false, true, "", "webhook.endpoint.created", "webhook_endpoint"},
		{"GET", base + "/endpoints/{endpointId}", "getWebhookEndpoint", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"PATCH", base + "/endpoints/{endpointId}", "updateWebhookEndpoint", false, "webhooks:write", httpapi.RateLimitNormal, true, false, "", "webhook.endpoint.updated", "webhook_endpoint"},
		{"DELETE", base + "/endpoints/{endpointId}", "deleteWebhookEndpoint", false, "webhooks:write", httpapi.RateLimitNormal, true, false, "", "webhook.endpoint.deleted", "webhook_endpoint"},
		{"POST", base + "/endpoints/{endpointId}/rotate-secret", "rotateWebhookEndpointSecret", false, "webhooks:write", httpapi.RateLimitNormal, false, true, "", "webhook.endpoint.secret_rotated", "webhook_endpoint"},
		{"GET", base + "/deliveries", "listWebhookDeliveries", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"GET", base + "/deliveries/{deliveryId}", "getWebhookDelivery", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"GET", base + "/deliveries/{deliveryId}/attempts", "listWebhookDeliveryAttempts", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"POST", base + "/deliveries/{deliveryId}/replay", "replayWebhookDelivery", false, "webhooks:write", httpapi.RateLimitNormal, true, false, "", "webhook.delivery.replayed", "webhook_delivery"},
		{"GET", base + "/inbound-sources", "listInboundSources", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"POST", base + "/inbound-sources", "createInboundSource", false, "webhooks:write", httpapi.RateLimitNormal, false, true, "", "webhook.inbound_source.created", "webhook_inbound_source"},
		{"GET", base + "/inbound-sources/{sourceId}", "getInboundSource", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"PATCH", base + "/inbound-sources/{sourceId}", "updateInboundSource", false, "webhooks:write", httpapi.RateLimitNormal, true, false, "", "webhook.inbound_source.updated", "webhook_inbound_source"},
		{"DELETE", base + "/inbound-sources/{sourceId}", "deleteInboundSource", false, "webhooks:write", httpapi.RateLimitNormal, true, false, "", "webhook.inbound_source.deleted", "webhook_inbound_source"},
		{"POST", base + "/inbound-sources/{sourceId}/rotate-secret", "rotateInboundSourceSecret", false, "webhooks:write", httpapi.RateLimitNormal, false, true, "", "webhook.inbound_source.secret_rotated", "webhook_inbound_source"},
		{"GET", base + "/inbound-sources/{sourceId}/events", "listInboundSourceEvents", false, "webhooks:read", httpapi.RateLimitNormal, false, false, "", "", ""},
		{"POST", base + "/inbound/{sourceId}", "receiveInboundWebhook", true, "", httpapi.RateLimitStrict, false, false, "webhook.received", "", ""},
	}

	ops := api.Operations()
	require.Len(t, ops, len(expected), "registered operation count")
	byID := make(map[string]httpapi.OperationInfo, len(ops))
	for _, op := range ops {
		byID[op.Op.OperationID] = op
	}
	for _, w := range expected {
		op, ok := byID[w.operationID]
		require.True(t, ok, "operationId %q is not registered", w.operationID)
		assert.Equal(t, w.method, op.Op.Method, w.operationID)
		assert.Equal(t, w.path, op.Op.Path, w.operationID)
		assert.Equal(t, w.public, op.Policy.Public, w.operationID)
		assert.Equal(t, w.permission, op.Policy.Permission, w.operationID)
		assert.Equal(t, w.rate, op.Policy.RateLimit, w.operationID)
		assert.Equal(t, w.idempotent, op.Policy.Idempotent, w.operationID)
		assert.Equal(t, w.noStore, op.Policy.NoStore, w.operationID)
		assert.Equal(t, w.tier1, op.Policy.Audit.Tier1Action, w.operationID)
		if w.guardAction == "" {
			assert.Nil(t, op.Policy.Audit.Guard, w.operationID)
			continue
		}
		require.NotNil(t, op.Policy.Audit.Guard, w.operationID)
		assert.Equal(t, w.guardAction, op.Policy.Audit.Guard.Action, w.operationID)
		assert.Equal(t, w.guardRes, op.Policy.Audit.Guard.Resource, w.operationID)
	}
}

func TestListEndpointsEnvelopeAndQuery(t *testing.T) {
	var (
		gotLimit     int
		gotCursor    string
		callReceived bool
	)
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000010")
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := &fakeService{listEndpoints: func(_ context.Context, limit int, startingAfter string) ([]webhookservice.Endpoint, bool, error) {
		callReceived = true
		gotLimit, gotCursor = limit, startingAfter
		return []webhookservice.Endpoint{*endpointFixture(id, created)}, true, nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodGet, "/api/v1/webhooks/endpoints?limit=2&startingAfter=abc", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.True(t, callReceived)
	assert.Equal(t, 2, gotLimit)
	assert.Equal(t, "abc", gotCursor)

	page := decode[dtoList[dto.WebhookEndpointResponse]](t, body)
	assert.Equal(t, "list", page.Object)
	assert.Equal(t, "/api/v1/webhooks/endpoints", page.URL)
	require.Len(t, page.Data, 1)
	assert.Equal(t, id, page.Data[0].ID)
	assert.True(t, page.HasMore)
	assert.NotEmpty(t, page.NextCursor)
}

// dtoList mirrors pagination.List for decoding in assertions.
type dtoList[T any] struct {
	Object     string `json:"object"`
	URL        string `json:"url"`
	Data       []T    `json:"data"`
	HasMore    bool   `json:"hasMore"`
	NextCursor string `json:"nextCursor"`
}

func TestCreateEndpointCreatedLocationAndSecret(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000011")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var got webhookservice.CreateEndpointInput
	svc := &fakeService{createEndpoint: func(_ context.Context, actorID uuid.UUID, in webhookservice.CreateEndpointInput) (*webhookservice.Endpoint, string, error) {
		assert.Equal(t, testActorID, actorID)
		got = in
		return endpointFixture(id, now), "whsec_new", nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/endpoints", map[string]any{
		"url": "https://receiver.example/hook", "status": "disabled", "eventTypes": []string{"file.*"},
	}, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "body: %s", body)
	assert.Equal(t, "/api/v1/webhooks/endpoints/"+id.String(), resp.Header.Get("Location"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "https://receiver.example/hook", got.URL)
	assert.Equal(t, "disabled", got.Status)

	created := decode[dto.WebhookEndpointSecretResponse](t, body)
	assert.Equal(t, id, created.ID)
	assert.Equal(t, "whsec_new", created.Secret)
}

func TestGetEndpointETagAndStaleIfMatch(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000012")
	updated := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	wantETag := strconv.Quote(strconv.FormatInt(updated.UnixNano(), 10))
	svc := &fakeService{
		getEndpoint: func(_ context.Context, got uuid.UUID) (*webhookservice.Endpoint, error) {
			require.Equal(t, id, got)
			return endpointFixture(id, updated), nil
		},
		// updateEndpoint is deliberately not installed: a stale If-Match must
		// be rejected before the service runs.
	}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodGet, "/api/v1/webhooks/endpoints/"+id.String(), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, wantETag, resp.Header.Get("ETag"))
	assert.Equal(t, id, decode[dto.WebhookEndpointResponse](t, body).ID)

	resp, body = do(t, fiberApp, http.MethodPatch, "/api/v1/webhooks/endpoints/"+id.String(), map[string]any{"status": "disabled"}, map[string]string{
		"If-Match":     `"stale"`,
		"Content-Type": "application/merge-patch+json",
	})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, "body: %s", body)
	problem := decode[apperror.ProblemDetails](t, body)
	assert.Equal(t, string(apperror.CodeResourcePreconditionFailed), problem.Code)
	assert.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))
}

func TestUpdateEndpointMergePatch(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000013")
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ifMatch := strconv.Quote(strconv.FormatInt(updated.UnixNano(), 10))
	wantETag := strconv.Quote(strconv.FormatInt(updated.Add(time.Minute).UnixNano(), 10))

	current := endpointFixture(id, updated)
	var patch webhookservice.UpdateEndpointInput
	svc := &fakeService{
		getEndpoint: func(context.Context, uuid.UUID) (*webhookservice.Endpoint, error) {
			return current, nil
		},
		updateEndpoint: func(_ context.Context, actorID, got uuid.UUID, in webhookservice.UpdateEndpointInput) (*webhookservice.Endpoint, error) {
			assert.Equal(t, testActorID, actorID)
			assert.Equal(t, id, got)
			patch = in
			next := endpointFixture(id, updated.Add(time.Minute))
			next.URL = in.URL
			next.Status = in.Status
			return next, nil
		},
	}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPatch, "/api/v1/webhooks/endpoints/"+id.String(), map[string]any{
		"url": "https://receiver.example/next", "status": "disabled",
	}, map[string]string{
		"If-Match":     ifMatch,
		"Content-Type": "application/merge-patch+json",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, wantETag, resp.Header.Get("ETag"))

	// The absent members keep the stored values; the present ones replace.
	assert.Equal(t, "https://receiver.example/next", patch.URL)
	assert.Equal(t, "orders", patch.Description)
	assert.Equal(t, []string{"file.uploaded"}, patch.EventTypes)
	assert.Equal(t, "disabled", patch.Status)
	require.NotNil(t, patch.MaxAttempts)
	assert.Equal(t, 8, *patch.MaxAttempts)
	require.NotNil(t, patch.TimeoutMS)
	assert.Equal(t, 10000, *patch.TimeoutMS)

	updatedBody := decode[dto.WebhookEndpointResponse](t, body)
	assert.Equal(t, "https://receiver.example/next", updatedBody.URL)
	assert.Equal(t, "disabled", updatedBody.Status)
}

func TestUpdateEndpointEmptyPatchIsNoOp(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000015")
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	current := endpointFixture(id, updated)
	svc := &fakeService{
		getEndpoint: func(context.Context, uuid.UUID) (*webhookservice.Endpoint, error) {
			return current, nil
		},
	}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPatch, "/api/v1/webhooks/endpoints/"+id.String(), map[string]any{}, map[string]string{
		"If-Match":     strconv.Quote(strconv.FormatInt(updated.UnixNano(), 10)),
		"Content-Type": "application/merge-patch+json",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, strconv.Quote(strconv.FormatInt(updated.UnixNano(), 10)), resp.Header.Get("ETag"))
	assert.Equal(t, "https://receiver.example/hook", decode[dto.WebhookEndpointResponse](t, body).URL)
}

func TestUpdateEndpointNullStatusRestoresActive(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000016")
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	current := endpointFixture(id, updated)
	var patch webhookservice.UpdateEndpointInput
	svc := &fakeService{
		getEndpoint: func(context.Context, uuid.UUID) (*webhookservice.Endpoint, error) {
			return current, nil
		},
		updateEndpoint: func(_ context.Context, _, _ uuid.UUID, in webhookservice.UpdateEndpointInput) (*webhookservice.Endpoint, error) {
			patch = in
			next := endpointFixture(id, updated.Add(time.Minute))
			next.Status = in.Status
			return next, nil
		},
	}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPatch, "/api/v1/webhooks/endpoints/"+id.String(), map[string]any{"status": nil}, map[string]string{
		"Content-Type": "application/merge-patch+json",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, webhookservice.StatusActive, patch.Status)
	assert.Equal(t, webhookservice.StatusActive, decode[dto.WebhookEndpointResponse](t, body).Status)
}

func TestDeleteEndpointNoContent(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000014")
	called := false
	svc := &fakeService{deleteEndpoint: func(_ context.Context, actorID, got uuid.UUID) error {
		called = true
		assert.Equal(t, testActorID, actorID)
		assert.Equal(t, id, got)
		return nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodDelete, "/api/v1/webhooks/endpoints/"+id.String(), nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "body: %s", body)
	assert.Empty(t, body)
	assert.True(t, called)
}

func TestRotateEndpointSecretReturnsOnlySecret(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000015")
	var gotExpire bool
	svc := &fakeService{rotateEndpointSecret: func(_ context.Context, actorID, got uuid.UUID, expirePrevious bool) (*webhookservice.Endpoint, string, error) {
		assert.Equal(t, testActorID, actorID)
		assert.Equal(t, id, got)
		gotExpire = expirePrevious
		return endpointFixture(id, time.Now()), "whsec_rotated", nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/endpoints/"+id.String()+"/rotate-secret", map[string]any{
		"expirePrevious": true,
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.True(t, gotExpire)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.Equal(t, map[string]any{"secret": "whsec_rotated"}, raw)
}

func TestListDeliveriesFiltersAndEnvelope(t *testing.T) {
	endpointID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000016")
	var gotEndpoint uuid.UUID
	var gotStatus string
	var gotLimit int
	svc := &fakeService{listDeliveries: func(_ context.Context, filter uuid.UUID, status string, limit int, _ string) ([]webhookservice.Delivery, bool, error) {
		gotEndpoint, gotStatus, gotLimit = filter, status, limit
		return nil, false, nil
	}}
	fiberApp, _ := newAPI(t, svc)

	path := "/api/v1/webhooks/deliveries?endpointId=" + endpointID.String() + "&status=dead&limit=5"
	resp, body := do(t, fiberApp, http.MethodGet, path, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Equal(t, endpointID, gotEndpoint)
	assert.Equal(t, "dead", gotStatus)
	assert.Equal(t, 5, gotLimit)

	page := decode[dtoList[dto.WebhookDeliveryResponse]](t, body)
	assert.Equal(t, "list", page.Object)
	assert.Equal(t, "/api/v1/webhooks/deliveries", page.URL)
	assert.Empty(t, page.Data, "an empty page serialises as []")
	assert.Contains(t, string(body), `"data":[]`)
	assert.False(t, page.HasMore)
	assert.Empty(t, page.NextCursor)
}

func TestGetDeliveryIncludesPayloadAndAttemptsList(t *testing.T) {
	deliveryID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000017")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	delivery := &webhookservice.Delivery{
		ID: deliveryID, TenantID: tenantID(), EndpointID: uuid.New(), EventID: uuid.New(),
		EventType: "file.uploaded", Status: webhookservice.DeliverySucceeded, Attempts: 1,
		MaxAttempts: 3, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		Payload: json.RawMessage(`{"fileId":7}`),
	}
	svc := &fakeService{
		getDelivery: func(_ context.Context, id uuid.UUID) (*webhookservice.Delivery, error) {
			require.Equal(t, deliveryID, id)
			return delivery, nil
		},
		listDeliveryAttempts: func(_ context.Context, id uuid.UUID) ([]webhookservice.DeliveryAttempt, error) {
			require.Equal(t, deliveryID, id)
			return []webhookservice.DeliveryAttempt{{ID: uuid.New(), DeliveryID: id, Attempt: 1, DurationMS: 12, CreatedAt: now}}, nil
		},
	}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodGet, "/api/v1/webhooks/deliveries/"+deliveryID.String(), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.JSONEq(t, `{"fileId":7}`, string(decode[dto.WebhookDeliveryResponse](t, body).Payload))

	resp, body = do(t, fiberApp, http.MethodGet, "/api/v1/webhooks/deliveries/"+deliveryID.String()+"/attempts", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	page := decode[dtoList[dto.WebhookAttemptResponse]](t, body)
	require.Len(t, page.Data, 1)
	assert.Equal(t, "/api/v1/webhooks/deliveries/"+deliveryID.String()+"/attempts", page.URL)
	assert.False(t, page.HasMore)
}

func TestReplayDeliveryAcceptedLocation(t *testing.T) {
	originalID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000018")
	newID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000019")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := &fakeService{replayDelivery: func(_ context.Context, actorID, got uuid.UUID) (*webhookservice.Delivery, error) {
		assert.Equal(t, testActorID, actorID)
		assert.Equal(t, originalID, got)
		return &webhookservice.Delivery{
			ID: newID, TenantID: tenantID(), EndpointID: uuid.New(), EventID: uuid.New(),
			EventType: "file.uploaded", Status: webhookservice.DeliveryPending,
			MaxAttempts: 3, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
			ReplayedFromID: &originalID,
		}, nil
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodPost, "/api/v1/webhooks/deliveries/"+originalID.String()+"/replay", nil, nil)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "body: %s", body)
	assert.Equal(t, "/api/v1/webhooks/deliveries/"+newID.String(), resp.Header.Get("Location"))
	replayed := decode[dto.WebhookDeliveryResponse](t, body)
	assert.Equal(t, newID, replayed.ID)
	require.NotNil(t, replayed.ReplayedFromID)
	assert.Equal(t, originalID, *replayed.ReplayedFromID)
}

func TestServiceErrorBecomesRFC9457Problem(t *testing.T) {
	id := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000020")
	svc := &fakeService{getEndpoint: func(context.Context, uuid.UUID) (*webhookservice.Endpoint, error) {
		return nil, apperror.NewNotFound("webhook endpoint not found")
	}}
	fiberApp, _ := newAPI(t, svc)

	resp, body := do(t, fiberApp, http.MethodGet, "/api/v1/webhooks/endpoints/"+id.String(), nil, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "body: %s", body)
	assert.Equal(t, "application/problem+json", resp.Header.Get("Content-Type"))
	problem := decode[apperror.ProblemDetails](t, body)
	assert.Equal(t, string(apperror.CodeResourceNotFound), problem.Code)
	assert.Equal(t, testRequestID, problem.RequestID)
	assert.Equal(t, "/api/v1/webhooks/endpoints/"+id.String(), problem.Instance)
}

func tenantID() uuid.UUID {
	return uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000002")
}
