package handler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
)

func TestToEndpointResponse(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	zone := time.FixedZone("UTC+7", 7*60*60)
	endpoint := webhookservice.Endpoint{
		ID:          testID(9),
		TenantID:    testID(2),
		URL:         "https://receiver.example/hook",
		Description: "orders",
		Status:      webhookservice.StatusActive,
		MaxAttempts: 3,
		TimeoutMS:   2000,
		CreatedAt:   utc.In(zone),
		UpdatedAt:   utc.In(zone),
	}

	data, err := json.Marshal(toEndpointResponse(&endpoint))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"tenantId":%q,"url":"https://receiver.example/hook","description":"orders","eventTypes":[],"status":"active","maxAttempts":3,"timeoutMs":2000,"createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, testID(9), testID(2)), string(data))

	deletedAt := utc
	endpoint.DeletedAt = &deletedAt
	resp := toEndpointResponse(&endpoint)
	require.NotNil(t, resp.DeletedAt)
	assert.True(t, deletedAt.Equal(*resp.DeletedAt))

	endpoint.EventTypes = []string{"file.*"}
	assert.Equal(t, []string{"file.*"}, toEndpointResponse(&endpoint).EventTypes)
}

func TestToDeliveryResponse(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	delivery := webhookservice.Delivery{
		ID:            testID(7),
		TenantID:      testID(2),
		EndpointID:    testID(9),
		EventID:       testID(11),
		EventType:     "file.uploaded",
		Status:        webhookservice.DeliveryPending,
		Attempts:      1,
		MaxAttempts:   3,
		NextAttemptAt: utc,
		CreatedAt:     utc,
		UpdatedAt:     utc,
	}

	data, err := json.Marshal(toDeliveryResponse(&delivery, false))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"tenantId":%q,"endpointId":%q,"eventId":%q,"eventType":"file.uploaded","status":"pending","attempts":1,"maxAttempts":3,"nextAttemptAt":"2026-01-02T03:04:05Z","createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, testID(7), testID(2), testID(9), testID(11)), string(data))

	statusCode := 204
	replayedFrom := testID(5)
	delivery.LastStatusCode = &statusCode
	delivery.LastError = "boom"
	delivery.DeliveredAt = &utc
	delivery.ReplayedFromID = &replayedFrom
	delivery.Payload = json.RawMessage(`{"fileId":7}`)

	data, err = json.Marshal(toDeliveryResponse(&delivery, true))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"tenantId":%q,"endpointId":%q,"eventId":%q,"eventType":"file.uploaded","status":"pending","attempts":1,"maxAttempts":3,"nextAttemptAt":"2026-01-02T03:04:05Z","lastStatusCode":204,"lastError":"boom","deliveredAt":"2026-01-02T03:04:05Z","replayedFromId":%q,"payload":{"fileId":7},"createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, testID(7), testID(2), testID(9), testID(11), testID(5)), string(data))
}

func TestToAttemptResponse(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	attempt := webhookservice.DeliveryAttempt{ID: testID(4), DeliveryID: testID(7), Attempt: 2, DurationMS: 15, CreatedAt: utc}

	data, err := json.Marshal(toAttemptResponse(&attempt))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"deliveryId":%q,"attempt":2,"durationMs":15,"createdAt":"2026-01-02T03:04:05Z"}`, testID(4), testID(7)), string(data))

	statusCode := 503
	attempt.StatusCode = &statusCode
	attempt.Error = "unavailable"
	resp := toAttemptResponse(&attempt)
	require.NotNil(t, resp.StatusCode)
	assert.Equal(t, statusCode, *resp.StatusCode)
	assert.Equal(t, "unavailable", resp.Error)
}

func TestToInboundSourceResponse(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	source := webhookservice.InboundSource{ID: testID(3), TenantID: testID(2), Name: "Stripe", Slug: "stripe", Status: webhookservice.StatusActive, CreatedAt: utc, UpdatedAt: utc}

	data, err := json.Marshal(toInboundSourceResponse(&source))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"tenantId":%q,"name":"Stripe","slug":"stripe","status":"active","receiverUrl":"/api/v1/webhooks/inbound/%s","createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, testID(3), testID(2), testID(3)), string(data))

	deletedAt := utc
	source.DeletedAt = &deletedAt
	resp := toInboundSourceResponse(&source)
	require.NotNil(t, resp.DeletedAt)
	assert.True(t, deletedAt.Equal(*resp.DeletedAt))
}

func TestToInboundEventAndReceipt(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	event := webhookservice.InboundEvent{
		ID:              testID(8),
		TenantID:        testID(2),
		SourceID:        testID(3),
		ExternalEventID: "msg_1",
		EventType:       "payment.succeeded",
		Payload:         json.RawMessage(`{"amount":10}`),
		ReceivedAt:      utc,
	}

	data, err := json.Marshal(toInboundEventResponse(&event))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"tenantId":%q,"sourceId":%q,"eventId":"msg_1","eventType":"payment.succeeded","payload":{"amount":10},"receivedAt":"2026-01-02T03:04:05Z","duplicate":false}`, testID(8), testID(2), testID(3)), string(data))

	receipt := webhookservice.InboundReceipt{ID: testID(8), TenantID: testID(2), SourceID: testID(3), EventID: "msg_1", EventType: "payment.succeeded", ReceivedAt: utc, Duplicate: true}
	data, err = json.Marshal(toInboundReceipt(&receipt))
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"id":%q,"tenantId":%q,"sourceId":%q,"eventId":"msg_1","eventType":"payment.succeeded","receivedAt":"2026-01-02T03:04:05Z","duplicate":true}`, testID(8), testID(2), testID(3)), string(data))
}

func TestCreateInputMapping(t *testing.T) {
	maxAttempts, timeoutMS := 4, 1500
	create := createEndpointInput(dto.CreateWebhookEndpointRequest{
		URL: "https://a.example", Description: "d", EventTypes: []string{"file.*"},
		Status: "disabled", MaxAttempts: &maxAttempts, TimeoutMS: &timeoutMS,
	})
	assert.Equal(t, webhookservice.CreateEndpointInput{
		URL: "https://a.example", Description: "d", EventTypes: []string{"file.*"},
		Status: "disabled", MaxAttempts: &maxAttempts, TimeoutMS: &timeoutMS,
	}, create)

	source := inboundSourceInputFromRequest(dto.CreateInboundSourceRequest{Name: "Stripe", Slug: "stripe", Status: "disabled"})
	assert.Equal(t, webhookservice.CreateInboundSourceInput{Name: "Stripe", Slug: "stripe", Status: "disabled"}, source)
}

func TestUpdatePatchDecoding(t *testing.T) {
	var req dto.UpdateWebhookEndpointRequest
	err := json.Unmarshal([]byte(`{"url":"https://b.example","description":null,"eventTypes":["user.*"],"status":"disabled","maxAttempts":5,"timeoutMs":null}`), &req)
	require.NoError(t, err)

	require.True(t, req.URL.Present)
	assert.False(t, req.URL.Null)
	assert.Equal(t, "https://b.example", req.URL.Value)

	require.True(t, req.Description.Present)
	assert.True(t, req.Description.Null)

	require.True(t, req.EventTypes.Present)
	assert.Equal(t, []string{"user.*"}, req.EventTypes.Value)

	require.True(t, req.MaxAttempts.Present)
	require.NotNil(t, req.MaxAttempts.Value)
	assert.Equal(t, 5, *req.MaxAttempts.Value)

	require.True(t, req.TimeoutMS.Present)
	assert.True(t, req.TimeoutMS.Null)
}

func TestApplyEndpointPatch(t *testing.T) {
	current := &webhookservice.Endpoint{
		URL: "https://a.example", Description: "d", EventTypes: []string{"file.*"},
		Status: webhookservice.StatusActive, MaxAttempts: 8, TimeoutMS: 10000,
	}

	// An absent member leaves the stored value untouched.
	unchanged := applyEndpointPatch(current, dto.UpdateWebhookEndpointRequest{})
	assert.Equal(t, "https://a.example", unchanged.URL)
	assert.Equal(t, "d", unchanged.Description)
	assert.Equal(t, []string{"file.*"}, unchanged.EventTypes)
	assert.Equal(t, webhookservice.StatusActive, unchanged.Status)
	require.NotNil(t, unchanged.MaxAttempts)
	assert.Equal(t, 8, *unchanged.MaxAttempts)
	require.NotNil(t, unchanged.TimeoutMS)
	assert.Equal(t, 10000, *unchanged.TimeoutMS)

	// A value replaces it and null restores the service default.
	maxAttempts := 3
	patched := applyEndpointPatch(current, dto.UpdateWebhookEndpointRequest{
		URL:         dto.Optional[string]{Present: true, Value: "https://b.example"},
		Description: dto.Optional[string]{Present: true, Null: true},
		EventTypes:  dto.Optional[[]string]{Present: true, Null: true},
		Status:      dto.Optional[string]{Present: true, Value: webhookservice.StatusDisabled},
		MaxAttempts: dto.Optional[*int]{Present: true, Value: &maxAttempts},
		TimeoutMS:   dto.Optional[*int]{Present: true, Null: true},
	})
	assert.Equal(t, "https://b.example", patched.URL)
	assert.Empty(t, patched.Description)
	assert.Nil(t, patched.EventTypes)
	assert.Equal(t, webhookservice.StatusDisabled, patched.Status)
	require.NotNil(t, patched.MaxAttempts)
	assert.Equal(t, 3, *patched.MaxAttempts)
	assert.Nil(t, patched.TimeoutMS)
}

func TestApplyInboundSourcePatch(t *testing.T) {
	current := &webhookservice.InboundSource{
		Name: "Stripe", Slug: "stripe", Status: webhookservice.StatusActive,
	}

	unchanged := applyInboundSourcePatch(current, dto.UpdateInboundSourceRequest{})
	assert.Equal(t, webhookservice.UpdateInboundSourceInput{
		Name: "Stripe", Slug: "stripe", Status: webhookservice.StatusActive,
	}, unchanged)

	patched := applyInboundSourcePatch(current, dto.UpdateInboundSourceRequest{
		Name:   dto.Optional[string]{Present: true, Value: "Payments"},
		Status: dto.Optional[string]{Present: true, Value: webhookservice.StatusDisabled},
	})
	assert.Equal(t, webhookservice.UpdateInboundSourceInput{
		Name: "Payments", Slug: "stripe", Status: webhookservice.StatusDisabled,
	}, patched)
}
