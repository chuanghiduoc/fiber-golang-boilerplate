// Package handler exposes the webhooks HTTP surface through huma operations.
// Handlers own only DTO mapping and response construction; tenant scoping,
// secrets, dispatch, and retries live in the service and repository layers.
package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// Handler serves tenant-scoped webhook administration and the public inbound
// receiver.
type Handler struct {
	service webhookservice.Service
}

// New returns a handler backed by svc.
func New(svc webhookservice.Service) *Handler {
	return &Handler{service: svc}
}

// ListEndpoints returns a page of the tenant's outbound endpoints.
func (h *Handler) ListEndpoints(ctx context.Context, in *listWebhookEndpointsInput) (*httpapi.ListOutput[dto.WebhookEndpointResponse], error) {
	items, hasMore, err := h.service.ListEndpoints(ctx, in.Limit, in.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := toEndpointResponses(items)
	next := pagination.NextCursor(out, func(e dto.WebhookEndpointResponse) (time.Time, uuid.UUID) {
		return e.CreatedAt, e.ID
	})
	return &httpapi.ListOutput[dto.WebhookEndpointResponse]{
		Body: listEnvelope(ctx, pathWebhookEndpoints, out, hasMore, next),
	}, nil
}

// CreateEndpoint creates an endpoint and returns its signing secret once.
func (h *Handler) CreateEndpoint(ctx context.Context, in *createWebhookEndpointInput) (*httpapi.Created[dto.WebhookEndpointSecretResponse], error) {
	endpoint, secret, err := h.service.CreateEndpoint(ctx, actorID(ctx), createEndpointInput(in.Body))
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated(endpointLocation(endpoint.ID), toEndpointSecretResponse(endpoint, secret)), nil
}

// GetEndpoint returns one endpoint with its version validator.
func (h *Handler) GetEndpoint(ctx context.Context, in *getWebhookEndpointInput) (*httpapi.ETagOutput[dto.WebhookEndpointResponse], error) {
	endpoint, err := h.service.GetEndpoint(ctx, in.EndpointID)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.WebhookEndpointResponse]{
		ETag: versionETag(endpoint.UpdatedAt),
		Body: toEndpointResponse(endpoint),
	}, nil
}

// UpdateEndpoint applies an RFC 7396 merge patch after checking If-Match.
func (h *Handler) UpdateEndpoint(ctx context.Context, in *updateWebhookEndpointInput) (*httpapi.ETagOutput[dto.WebhookEndpointResponse], error) {
	current, err := h.service.GetEndpoint(ctx, in.EndpointID)
	if err != nil {
		return nil, err
	}
	if err := httpapi.CheckIfMatchValue(in.IfMatch, versionETag(current.UpdatedAt)); err != nil {
		return nil, err
	}
	if endpointPatchEmpty(in.Body) {
		return &httpapi.ETagOutput[dto.WebhookEndpointResponse]{
			ETag: versionETag(current.UpdatedAt),
			Body: toEndpointResponse(current),
		}, nil
	}
	update := applyEndpointPatch(current, in.Body)
	if in.IfMatch != "" {
		expected := current.UpdatedAt
		update.ExpectedUpdatedAt = &expected
	}
	endpoint, err := h.service.UpdateEndpoint(ctx, actorID(ctx), in.EndpointID, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.WebhookEndpointResponse]{
		ETag: versionETag(endpoint.UpdatedAt),
		Body: toEndpointResponse(endpoint),
	}, nil
}

// DeleteEndpoint soft-deletes an endpoint.
func (h *Handler) DeleteEndpoint(ctx context.Context, in *deleteWebhookEndpointInput) (*httpapi.Empty, error) {
	if err := h.service.DeleteEndpoint(ctx, actorID(ctx), in.EndpointID); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// RotateEndpointSecret replaces the active signing secret and returns it once.
func (h *Handler) RotateEndpointSecret(ctx context.Context, in *rotateEndpointSecretInput) (*httpapi.Output[dto.SecretResponse], error) {
	expirePrevious := in.Body != nil && in.Body.ExpirePrevious
	_, secret, err := h.service.RotateEndpointSecret(ctx, actorID(ctx), in.EndpointID, expirePrevious)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.SecretResponse]{Body: dto.SecretResponse{Secret: secret}}, nil
}
