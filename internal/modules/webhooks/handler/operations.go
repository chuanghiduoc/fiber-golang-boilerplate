package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// Collection paths used in Location headers and list envelopes when the
// request context does not carry the path (unit-test APIs without the
// request-ID middleware).
const (
	pathWebhookEndpoints    = httpapi.APIPrefix + "/webhooks/endpoints"
	pathWebhookDeliveries   = httpapi.APIPrefix + "/webhooks/deliveries"
	pathInboundSources      = httpapi.APIPrefix + "/webhooks/inbound-sources"
	pathInboundSourceEvents = pathInboundSources + "/%s/events"
	pathWebhookEndpointByID = pathWebhookEndpoints + "/%s"
	pathWebhookDeliveryByID = pathWebhookDeliveries + "/%s"
	pathWebhookAttempts     = pathWebhookDeliveries + "/%s/attempts"
	pathInboundSourceByID   = pathInboundSources + "/%s"
)

// actorID returns the authenticated caller's user id. The policy chain installs
// the subject before the handler runs; an absent subject is the zero id.
func actorID(ctx context.Context) uuid.UUID {
	if subject, ok := authz.SubjectFromContext(ctx); ok {
		return subject.UserID
	}
	return uuid.Nil
}

// versionETag is the strong entity-tag for a config resource: its last update
// timestamp. It is quoted so the response header is a valid entity-tag;
// CheckIfMatchValue normalizes it again before comparing.
func versionETag(updatedAt time.Time) string {
	if updatedAt.IsZero() {
		return ""
	}
	return strconv.Quote(strconv.FormatInt(updatedAt.UTC().UnixNano(), 10))
}

// collectionURL returns the request path for the list envelope, falling back to
// the canonical collection path.
func collectionURL(ctx context.Context, fallback string) string {
	if path := requestctx.Path(ctx); path != "" {
		return path
	}
	return fallback
}

func endpointLocation(id uuid.UUID) string {
	return fmt.Sprintf(pathWebhookEndpointByID, id)
}

func deliveryLocation(id uuid.UUID) string {
	return fmt.Sprintf(pathWebhookDeliveryByID, id)
}

func inboundSourceLocation(id uuid.UUID) string {
	return fmt.Sprintf(pathInboundSourceByID, id)
}

func inboundSourceEventsPath(id uuid.UUID) string {
	return fmt.Sprintf(pathInboundSourceEvents, id)
}

// Page inputs.

type listWebhookEndpointsInput struct {
	Limit         int    `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Page size."`
	StartingAfter string `query:"startingAfter" maxLength:"512" doc:"Opaque cursor from the previous page."`
}

type listWebhookDeliveriesInput struct {
	EndpointID    uuid.UUID `query:"endpointId" format:"uuid" doc:"Only deliveries of this endpoint."`
	Status        string    `query:"status" enum:"pending,succeeded,dead" doc:"Only deliveries in this state."`
	Limit         int       `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Page size."`
	StartingAfter string    `query:"startingAfter" maxLength:"512" doc:"Opaque cursor from the previous page."`
}

type listInboundSourcesInput struct {
	Limit         int    `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Page size."`
	StartingAfter string `query:"startingAfter" maxLength:"512" doc:"Opaque cursor from the previous page."`
}

type listInboundSourceEventsInput struct {
	SourceID      uuid.UUID `path:"sourceId" format:"uuid" doc:"Inbound source ID."`
	Limit         int       `query:"limit" default:"20" minimum:"1" maximum:"100" doc:"Page size."`
	StartingAfter string    `query:"startingAfter" maxLength:"512" doc:"Opaque cursor from the previous page."`
}

// Endpoint inputs.

type createWebhookEndpointInput struct {
	Body dto.CreateWebhookEndpointRequest
}

type getWebhookEndpointInput struct {
	EndpointID uuid.UUID `path:"endpointId" format:"uuid" doc:"Webhook endpoint ID."`
}

type updateWebhookEndpointInput struct {
	EndpointID     uuid.UUID                        `path:"endpointId" format:"uuid" doc:"Webhook endpoint ID."`
	IfMatch        string                           `header:"If-Match" doc:"Current ETag; a stale value is rejected with 412."`
	IdempotencyKey string                           `header:"Idempotency-Key" doc:"Optional retry key; a replay returns the stored outcome."`
	Body           dto.UpdateWebhookEndpointRequest `contentType:"application/merge-patch+json"`
}

type deleteWebhookEndpointInput struct {
	EndpointID     uuid.UUID `path:"endpointId" format:"uuid" doc:"Webhook endpoint ID."`
	IdempotencyKey string    `header:"Idempotency-Key" doc:"Optional retry key; a replay returns the stored outcome."`
}

type rotateEndpointSecretInput struct {
	EndpointID uuid.UUID `path:"endpointId" format:"uuid" doc:"Webhook endpoint ID."`
	Body       *dto.RotateSecretRequest
}

// Delivery inputs.

type getWebhookDeliveryInput struct {
	DeliveryID uuid.UUID `path:"deliveryId" format:"uuid" doc:"Webhook delivery ID."`
}

type listWebhookDeliveryAttemptsInput struct {
	DeliveryID uuid.UUID `path:"deliveryId" format:"uuid" doc:"Webhook delivery ID."`
}

type replayWebhookDeliveryInput struct {
	DeliveryID     uuid.UUID `path:"deliveryId" format:"uuid" doc:"Webhook delivery ID."`
	IdempotencyKey string    `header:"Idempotency-Key" doc:"Optional retry key; a replay returns the stored outcome."`
}

// Inbound source inputs.

type createInboundSourceInput struct {
	Body dto.CreateInboundSourceRequest
}

type getInboundSourceInput struct {
	SourceID uuid.UUID `path:"sourceId" format:"uuid" doc:"Inbound source ID."`
}

type updateInboundSourceInput struct {
	SourceID       uuid.UUID                      `path:"sourceId" format:"uuid" doc:"Inbound source ID."`
	IfMatch        string                         `header:"If-Match" doc:"Current ETag; a stale value is rejected with 412."`
	IdempotencyKey string                         `header:"Idempotency-Key" doc:"Optional retry key; a replay returns the stored outcome."`
	Body           dto.UpdateInboundSourceRequest `contentType:"application/merge-patch+json"`
}

type deleteInboundSourceInput struct {
	SourceID       uuid.UUID `path:"sourceId" format:"uuid" doc:"Inbound source ID."`
	IdempotencyKey string    `header:"Idempotency-Key" doc:"Optional retry key; a replay returns the stored outcome."`
}

type rotateInboundSourceSecretInput struct {
	SourceID uuid.UUID `path:"sourceId" format:"uuid" doc:"Inbound source ID."`
	Body     *dto.RotateSecretRequest
}

// Public receiver input and output.

type receiveInboundWebhookInput struct {
	SourceID         uuid.UUID       `path:"sourceId" format:"uuid" doc:"Inbound source ID."`
	WebhookID        string          `header:"webhook-id" required:"true" doc:"Standard Webhooks message id (deduplication key)."`
	WebhookTimestamp string          `header:"webhook-timestamp" required:"true" doc:"Unix seconds covered by the signature."`
	WebhookSignature string          `header:"webhook-signature" required:"true" doc:"Space-separated v1,<base64 HMAC-SHA256> signatures."`
	Body             json.RawMessage `doc:"Signed JSON envelope; any JSON value is stored verbatim."`
}

// receiveInboundWebhookOutput carries the acknowledgement and the status the
// service selected: 202 for a new event, 200 for a duplicate.
type receiveInboundWebhookOutput struct {
	Status int `json:"-"`
	Body   dto.InboundReceipt
}

// HTTPStatus documents the new-event success status.
func (*receiveInboundWebhookOutput) HTTPStatus() int { return 202 }

// listEnvelope builds the cursor envelope for one page.
func listEnvelope[T any](ctx context.Context, fallback string, data []T, hasMore bool, nextCursor string) pagination.List[T] {
	return pagination.NewList(collectionURL(ctx, fallback), data, hasMore, nextCursor)
}
