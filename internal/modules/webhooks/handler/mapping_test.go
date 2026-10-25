package handler

import (
	"encoding/json"
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
		ID:          9,
		TenantID:    2,
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
	assert.JSONEq(t, `{"id":9,"tenantId":2,"url":"https://receiver.example/hook","description":"orders","eventTypes":[],"status":"active","maxAttempts":3,"timeoutMs":2000,"createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, string(data))

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
		ID:            7,
		TenantID:      2,
		EndpointID:    9,
		EventID:       "evt_1",
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
	assert.JSONEq(t, `{"id":7,"tenantId":2,"endpointId":9,"eventId":"evt_1","eventType":"file.uploaded","status":"pending","attempts":1,"maxAttempts":3,"nextAttemptAt":"2026-01-02T03:04:05Z","createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, string(data))

	statusCode := 204
	replayedFrom := int64(5)
	delivery.LastStatusCode = &statusCode
	delivery.LastError = "boom"
	delivery.DeliveredAt = &utc
	delivery.ReplayedFromID = &replayedFrom
	delivery.Payload = json.RawMessage(`{"fileId":7}`)

	data, err = json.Marshal(toDeliveryResponse(&delivery, true))
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":7,"tenantId":2,"endpointId":9,"eventId":"evt_1","eventType":"file.uploaded","status":"pending","attempts":1,"maxAttempts":3,"nextAttemptAt":"2026-01-02T03:04:05Z","lastStatusCode":204,"lastError":"boom","deliveredAt":"2026-01-02T03:04:05Z","replayedFromId":5,"payload":{"fileId":7},"createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, string(data))
}

func TestToAttemptResponse(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	attempt := webhookservice.DeliveryAttempt{ID: 4, DeliveryID: 7, Attempt: 2, DurationMS: 15, CreatedAt: utc}

	data, err := json.Marshal(toAttemptResponse(&attempt))
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":4,"deliveryId":7,"attempt":2,"durationMs":15,"createdAt":"2026-01-02T03:04:05Z"}`, string(data))

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
	source := webhookservice.InboundSource{ID: 3, TenantID: 2, Name: "Stripe", Slug: "stripe", Status: webhookservice.StatusActive, CreatedAt: utc, UpdatedAt: utc}

	data, err := json.Marshal(toInboundSourceResponse(&source))
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":3,"tenantId":2,"name":"Stripe","slug":"stripe","status":"active","receiverUrl":"/api/v1/webhooks/inbound/3","createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z"}`, string(data))

	deletedAt := utc
	source.DeletedAt = &deletedAt
	resp := toInboundSourceResponse(&source)
	require.NotNil(t, resp.DeletedAt)
	assert.True(t, deletedAt.Equal(*resp.DeletedAt))
}

func TestToInboundEventAndReceipt(t *testing.T) {
	utc := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	event := webhookservice.InboundEvent{
		ID:              8,
		TenantID:        2,
		SourceID:        3,
		ExternalEventID: "msg_1",
		EventType:       "payment.succeeded",
		Payload:         json.RawMessage(`{"amount":10}`),
		ReceivedAt:      utc,
	}

	data, err := json.Marshal(toInboundEventResponse(&event))
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":8,"tenantId":2,"sourceId":3,"eventId":"msg_1","eventType":"payment.succeeded","payload":{"amount":10},"receivedAt":"2026-01-02T03:04:05Z","duplicate":false}`, string(data))

	receipt := webhookservice.InboundReceipt{ID: 8, TenantID: 2, SourceID: 3, EventID: "msg_1", EventType: "payment.succeeded", ReceivedAt: utc, Duplicate: true}
	data, err = json.Marshal(toInboundReceipt(&receipt))
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":8,"tenantId":2,"sourceId":3,"eventId":"msg_1","eventType":"payment.succeeded","receivedAt":"2026-01-02T03:04:05Z","duplicate":true}`, string(data))
}

func TestInputMapping(t *testing.T) {
	maxAttempts, timeoutMS := 4, 1500
	create := createEndpointInput(dto.CreateEndpointRequest{URL: "https://a.example", Description: "d", EventTypes: []string{"file.*"}, MaxAttempts: &maxAttempts, TimeoutMS: &timeoutMS})
	assert.Equal(t, webhookservice.CreateEndpointInput{URL: "https://a.example", Description: "d", EventTypes: []string{"file.*"}, MaxAttempts: &maxAttempts, TimeoutMS: &timeoutMS}, create)

	update := updateEndpointInput(dto.UpdateEndpointRequest{URL: "https://b.example", Description: "e", EventTypes: []string{"user.*"}, Status: "disabled", MaxAttempts: &maxAttempts, TimeoutMS: &timeoutMS})
	assert.Equal(t, webhookservice.UpdateEndpointInput{URL: "https://b.example", Description: "e", EventTypes: []string{"user.*"}, Status: "disabled", MaxAttempts: &maxAttempts, TimeoutMS: &timeoutMS}, update)

	assert.Equal(t, webhookservice.CreateInboundSourceInput{Name: "Stripe", Slug: "stripe"}, createInboundSourceInput(dto.CreateInboundSourceRequest{Name: "Stripe", Slug: "stripe"}))
	assert.Equal(t, webhookservice.UpdateInboundSourceInput{Name: "Stripe", Slug: "stripe", Status: "active"}, updateInboundSourceInput(dto.UpdateInboundSourceRequest{Name: "Stripe", Slug: "stripe", Status: "active"}))
}
