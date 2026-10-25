package handler

import (
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
)

func createEndpointInput(req dto.CreateEndpointRequest) webhookservice.CreateEndpointInput {
	return webhookservice.CreateEndpointInput{
		URL:         req.URL,
		Description: req.Description,
		EventTypes:  req.EventTypes,
		MaxAttempts: req.MaxAttempts,
		TimeoutMS:   req.TimeoutMS,
	}
}

func updateEndpointInput(req dto.UpdateEndpointRequest) webhookservice.UpdateEndpointInput {
	return webhookservice.UpdateEndpointInput{
		URL:         req.URL,
		Description: req.Description,
		EventTypes:  req.EventTypes,
		Status:      req.Status,
		MaxAttempts: req.MaxAttempts,
		TimeoutMS:   req.TimeoutMS,
	}
}

func createInboundSourceInput(req dto.CreateInboundSourceRequest) webhookservice.CreateInboundSourceInput {
	return webhookservice.CreateInboundSourceInput{Name: req.Name, Slug: req.Slug}
}

func updateInboundSourceInput(req dto.UpdateInboundSourceRequest) webhookservice.UpdateInboundSourceInput {
	return webhookservice.UpdateInboundSourceInput{Name: req.Name, Slug: req.Slug, Status: req.Status}
}

func toEndpointResponse(endpoint *webhookservice.Endpoint) dto.EndpointResponse {
	resp := dto.EndpointResponse{
		ID:          endpoint.ID,
		TenantID:    endpoint.TenantID,
		URL:         endpoint.URL,
		Description: endpoint.Description,
		EventTypes:  endpoint.EventTypes,
		Status:      endpoint.Status,
		MaxAttempts: endpoint.MaxAttempts,
		TimeoutMS:   endpoint.TimeoutMS,
		CreatedAt:   endpoint.CreatedAt.UTC(),
		UpdatedAt:   endpoint.UpdatedAt.UTC(),
	}
	if resp.EventTypes == nil {
		resp.EventTypes = []string{}
	}
	if endpoint.DeletedAt != nil {
		deletedAt := endpoint.DeletedAt.UTC()
		resp.DeletedAt = &deletedAt
	}
	return resp
}

func toEndpointResponses(endpoints []webhookservice.Endpoint) []dto.EndpointResponse {
	out := make([]dto.EndpointResponse, len(endpoints))
	for i := range endpoints {
		out[i] = toEndpointResponse(&endpoints[i])
	}
	return out
}

func toEndpointSecretResponse(endpoint *webhookservice.Endpoint, secret string) dto.EndpointSecretResponse {
	return dto.EndpointSecretResponse{EndpointResponse: toEndpointResponse(endpoint), Secret: secret}
}

func toDeliveryResponse(delivery *webhookservice.Delivery, includePayload bool) dto.DeliveryResponse {
	resp := dto.DeliveryResponse{
		ID:             delivery.ID,
		TenantID:       delivery.TenantID,
		EndpointID:     delivery.EndpointID,
		EventID:        delivery.EventID,
		EventType:      delivery.EventType,
		Status:         delivery.Status,
		Attempts:       delivery.Attempts,
		MaxAttempts:    delivery.MaxAttempts,
		NextAttemptAt:  delivery.NextAttemptAt.UTC(),
		LastStatusCode: delivery.LastStatusCode,
		LastError:      delivery.LastError,
		CreatedAt:      delivery.CreatedAt.UTC(),
		UpdatedAt:      delivery.UpdatedAt.UTC(),
	}
	if delivery.DeliveredAt != nil {
		at := delivery.DeliveredAt.UTC()
		resp.DeliveredAt = &at
	}
	if delivery.ReplayedFromID != nil {
		id := *delivery.ReplayedFromID
		resp.ReplayedFromID = &id
	}
	if includePayload {
		resp.Payload = delivery.Payload
	}
	return resp
}

func toDeliveryResponses(deliveries []webhookservice.Delivery) []dto.DeliveryResponse {
	out := make([]dto.DeliveryResponse, len(deliveries))
	for i := range deliveries {
		out[i] = toDeliveryResponse(&deliveries[i], false)
	}
	return out
}

func toAttemptResponse(attempt *webhookservice.DeliveryAttempt) dto.DeliveryAttemptResponse {
	return dto.DeliveryAttemptResponse{
		ID:         attempt.ID,
		DeliveryID: attempt.DeliveryID,
		Attempt:    attempt.Attempt,
		StatusCode: attempt.StatusCode,
		DurationMS: attempt.DurationMS,
		Error:      attempt.Error,
		CreatedAt:  attempt.CreatedAt.UTC(),
	}
}

func toAttemptResponses(attempts []webhookservice.DeliveryAttempt) []dto.DeliveryAttemptResponse {
	out := make([]dto.DeliveryAttemptResponse, len(attempts))
	for i := range attempts {
		out[i] = toAttemptResponse(&attempts[i])
	}
	return out
}

func toInboundSourceResponse(source *webhookservice.InboundSource) dto.InboundSourceResponse {
	resp := dto.InboundSourceResponse{
		ID:          source.ID,
		TenantID:    source.TenantID,
		Name:        source.Name,
		Slug:        source.Slug,
		Status:      source.Status,
		ReceiverURL: fmt.Sprintf("/api/v1/webhooks/inbound/%d", source.ID),
		CreatedAt:   source.CreatedAt.UTC(),
		UpdatedAt:   source.UpdatedAt.UTC(),
	}
	if source.DeletedAt != nil {
		deletedAt := source.DeletedAt.UTC()
		resp.DeletedAt = &deletedAt
	}
	return resp
}

func toInboundSourceResponses(sources []webhookservice.InboundSource) []dto.InboundSourceResponse {
	out := make([]dto.InboundSourceResponse, len(sources))
	for i := range sources {
		out[i] = toInboundSourceResponse(&sources[i])
	}
	return out
}

func toInboundSourceSecretResponse(source *webhookservice.InboundSource, secret string) dto.InboundSourceSecretResponse {
	return dto.InboundSourceSecretResponse{InboundSourceResponse: toInboundSourceResponse(source), Secret: secret}
}

func toInboundEventResponse(event *webhookservice.InboundEvent) dto.InboundEventResponse {
	return dto.InboundEventResponse{
		ID:              event.ID,
		TenantID:        event.TenantID,
		SourceID:        event.SourceID,
		ExternalEventID: event.ExternalEventID,
		EventType:       event.EventType,
		Payload:         event.Payload,
		ReceivedAt:      event.ReceivedAt.UTC(),
	}
}

func toInboundEventResponses(events []webhookservice.InboundEvent) []dto.InboundEventResponse {
	out := make([]dto.InboundEventResponse, len(events))
	for i := range events {
		out[i] = toInboundEventResponse(&events[i])
	}
	return out
}

func toInboundReceipt(receipt *webhookservice.InboundReceipt) dto.InboundReceipt {
	return dto.InboundReceipt{
		ID:         receipt.ID,
		TenantID:   receipt.TenantID,
		SourceID:   receipt.SourceID,
		EventID:    receipt.EventID,
		EventType:  receipt.EventType,
		ReceivedAt: receipt.ReceivedAt.UTC(),
		Duplicate:  receipt.Duplicate,
	}
}
