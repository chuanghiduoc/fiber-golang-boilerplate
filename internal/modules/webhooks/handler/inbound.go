package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// ListInboundSources returns a page of the tenant's inbound sources.
func (h *Handler) ListInboundSources(ctx context.Context, in *listInboundSourcesInput) (*httpapi.ListOutput[dto.InboundSourceResponse], error) {
	items, hasMore, err := h.service.ListInboundSources(ctx, in.Limit, in.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := toInboundSourceResponses(items)
	next := pagination.NextCursor(out, func(s dto.InboundSourceResponse) (time.Time, uuid.UUID) {
		return s.CreatedAt, s.ID
	})
	return &httpapi.ListOutput[dto.InboundSourceResponse]{
		Body: listEnvelope(ctx, pathInboundSources, out, hasMore, next),
	}, nil
}

// CreateInboundSource creates a receiver and returns its verification secret
// once.
func (h *Handler) CreateInboundSource(ctx context.Context, in *createInboundSourceInput) (*httpapi.Created[dto.InboundSourceSecretResponse], error) {
	source, secret, err := h.service.CreateInboundSource(ctx, actorID(ctx), inboundSourceInputFromRequest(in.Body))
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated(inboundSourceLocation(source.ID), toInboundSourceSecretResponse(source, secret)), nil
}

// GetInboundSource returns one source with its version validator.
func (h *Handler) GetInboundSource(ctx context.Context, in *getInboundSourceInput) (*httpapi.ETagOutput[dto.InboundSourceResponse], error) {
	source, err := h.service.GetInboundSource(ctx, in.SourceID)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.InboundSourceResponse]{
		ETag: versionETag(source.UpdatedAt),
		Body: toInboundSourceResponse(source),
	}, nil
}

// UpdateInboundSource applies an RFC 7396 merge patch after checking If-Match.
func (h *Handler) UpdateInboundSource(ctx context.Context, in *updateInboundSourceInput) (*httpapi.ETagOutput[dto.InboundSourceResponse], error) {
	current, err := h.service.GetInboundSource(ctx, in.SourceID)
	if err != nil {
		return nil, err
	}
	if err := httpapi.CheckIfMatchValue(in.IfMatch, versionETag(current.UpdatedAt)); err != nil {
		return nil, err
	}
	if inboundSourcePatchEmpty(in.Body) {
		return &httpapi.ETagOutput[dto.InboundSourceResponse]{
			ETag: versionETag(current.UpdatedAt),
			Body: toInboundSourceResponse(current),
		}, nil
	}
	update := applyInboundSourcePatch(current, in.Body)
	if in.IfMatch != "" {
		expected := current.UpdatedAt
		update.ExpectedUpdatedAt = &expected
	}
	source, err := h.service.UpdateInboundSource(ctx, actorID(ctx), in.SourceID, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.InboundSourceResponse]{
		ETag: versionETag(source.UpdatedAt),
		Body: toInboundSourceResponse(source),
	}, nil
}

// DeleteInboundSource soft-deletes a receiver.
func (h *Handler) DeleteInboundSource(ctx context.Context, in *deleteInboundSourceInput) (*httpapi.Empty, error) {
	if err := h.service.DeleteInboundSource(ctx, actorID(ctx), in.SourceID); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// RotateInboundSourceSecret replaces the verification secret and returns it
// once.
func (h *Handler) RotateInboundSourceSecret(ctx context.Context, in *rotateInboundSourceSecretInput) (*httpapi.Output[dto.SecretResponse], error) {
	expirePrevious := in.Body != nil && in.Body.ExpirePrevious
	_, secret, err := h.service.RotateInboundSourceSecret(ctx, actorID(ctx), in.SourceID, expirePrevious)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.SecretResponse]{Body: dto.SecretResponse{Secret: secret}}, nil
}

// ListInboundSourceEvents returns a page of deduplicated events received by
// one source.
func (h *Handler) ListInboundSourceEvents(ctx context.Context, in *listInboundSourceEventsInput) (*httpapi.ListOutput[dto.InboundEventResponse], error) {
	items, hasMore, err := h.service.ListInboundEvents(ctx, in.SourceID, in.Limit, in.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := toInboundEventResponses(items)
	next := pagination.NextCursor(out, func(e dto.InboundEventResponse) (time.Time, uuid.UUID) {
		return e.ReceivedAt, e.ID
	})
	return &httpapi.ListOutput[dto.InboundEventResponse]{
		Body: listEnvelope(ctx, inboundSourceEventsPath(in.SourceID), out, hasMore, next),
	}, nil
}

// ReceiveInbound authenticates and stores a public Standard Webhooks delivery.
// A new event is 202; a repeated (source, webhook-id) is 200 with
// duplicate=true.
func (h *Handler) ReceiveInbound(ctx context.Context, in *receiveInboundWebhookInput) (*receiveInboundWebhookOutput, error) {
	headers := http.Header{}
	headers.Set(webhook.HeaderID, in.WebhookID)
	headers.Set(webhook.HeaderTimestamp, in.WebhookTimestamp)
	headers.Set(webhook.HeaderSignature, in.WebhookSignature)

	receipt, err := h.service.ReceiveInbound(ctx, in.SourceID, headers, in.Body)
	if err != nil {
		return nil, err
	}
	status := http.StatusAccepted
	if receipt.Duplicate {
		status = http.StatusOK
	}
	return &receiveInboundWebhookOutput{Status: status, Body: toInboundReceipt(receipt)}, nil
}
