package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const (
	maxInboundPayloadBytes = 1 << 20
	defaultMaxAttempts     = 8
	defaultTimeoutMS       = 10_000
	maxEventIDLen          = 255
	secretPrefixEndpoint   = "whsec_"
	secretPrefixInbound    = "whin_"
	auditTimeout           = 2 * time.Second
)

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Options configures service defaults. Zero values fall back to safe defaults.
type Options struct {
	DefaultMaxAttempts int
	DefaultTimeoutMS   int32
	TimestampTolerance time.Duration
}

// Service is the webhooks application surface: tenant-scoped endpoint CRUD,
// secret rotation, delivery history/replay, inbound source CRUD, and the
// public inbound receiver.
type Service interface {
	CreateEndpoint(ctx context.Context, actorID int64, req dto.CreateEndpointRequest) (*dto.EndpointSecretResponse, error)
	UpdateEndpoint(ctx context.Context, actorID, id int64, req dto.UpdateEndpointRequest) (*dto.EndpointResponse, error)
	DeleteEndpoint(ctx context.Context, actorID, id int64) error
	GetEndpoint(ctx context.Context, id int64) (*dto.EndpointResponse, error)
	ListEndpoints(ctx context.Context, limit int, startingAfter string) ([]dto.EndpointResponse, bool, error)
	RotateEndpointSecret(ctx context.Context, actorID, id int64) (*dto.EndpointSecretResponse, error)

	ListDeliveries(ctx context.Context, endpointID int64, status string, limit int, startingAfter string) ([]dto.DeliveryResponse, bool, error)
	GetDelivery(ctx context.Context, id int64) (*dto.DeliveryResponse, error)
	ListDeliveryAttempts(ctx context.Context, id int64) ([]dto.DeliveryAttemptResponse, error)
	ReplayDelivery(ctx context.Context, actorID, id int64) (*dto.DeliveryResponse, error)

	CreateInboundSource(ctx context.Context, actorID int64, req dto.CreateInboundSourceRequest) (*dto.InboundSourceSecretResponse, error)
	UpdateInboundSource(ctx context.Context, actorID, id int64, req dto.UpdateInboundSourceRequest) (*dto.InboundSourceResponse, error)
	DeleteInboundSource(ctx context.Context, actorID, id int64) error
	GetInboundSource(ctx context.Context, id int64) (*dto.InboundSourceResponse, error)
	ListInboundSources(ctx context.Context, limit int, startingAfter string) ([]dto.InboundSourceResponse, bool, error)
	RotateInboundSourceSecret(ctx context.Context, actorID, id int64) (*dto.InboundSourceSecretResponse, error)
	ListInboundEvents(ctx context.Context, sourceID int64, limit int, startingAfter string) ([]dto.InboundEventResponse, bool, error)
	ReceiveInbound(ctx context.Context, sourceID int64, headers http.Header, body []byte) (*dto.InboundReceipt, error)

	PurgeRetention(ctx context.Context, retentionDays int32) (deliveries, events int64, err error)
}

type service struct {
	endpoints  repository.EndpointStore
	deliveries repository.DeliveryStore
	inbound    repository.InboundStore
	codec      *SecretCodec
	audit      audit.Recorder
	opts       Options
	now        func() time.Time
	urlGuard   *webhook.Guard
}

// New returns the webhooks service. audit may be nil (Noop semantics).
func New(
	endpoints repository.EndpointStore,
	deliveries repository.DeliveryStore,
	inbound repository.InboundStore,
	codec *SecretCodec,
	recorder audit.Recorder,
	opts Options,
) Service {
	if opts.DefaultMaxAttempts <= 0 {
		opts.DefaultMaxAttempts = defaultMaxAttempts
	}
	if opts.DefaultTimeoutMS <= 0 {
		opts.DefaultTimeoutMS = defaultTimeoutMS
	}
	if opts.TimestampTolerance <= 0 {
		opts.TimestampTolerance = webhook.DefaultTolerance
	}
	guard, _ := webhook.NewGuard(nil)
	return &service{
		endpoints:  endpoints,
		deliveries: deliveries,
		inbound:    inbound,
		codec:      codec,
		audit:      recorder,
		opts:       opts,
		now:        func() time.Time { return time.Now().UTC() },
		urlGuard:   guard,
	}
}

// CreateEndpoint validates configuration, seals a fresh signing secret, and
// returns the plaintext secret exactly once.
func (s *service) CreateEndpoint(ctx context.Context, actorID int64, req dto.CreateEndpointRequest) (*dto.EndpointSecretResponse, error) {
	if err := s.validateURL(req.URL); err != nil {
		return nil, err
	}
	eventTypes, err := normalizeEventTypes(req.EventTypes)
	if err != nil {
		return nil, err
	}
	secret, err := GenerateSecret(secretPrefixEndpoint)
	if err != nil {
		return nil, apperror.NewInternal("failed to generate webhook secret")
	}
	ciphertext, err := s.codec.Seal([]byte(secret))
	if err != nil {
		return nil, apperror.NewInternal("failed to protect webhook secret")
	}
	maxAttempts := s.maxAttempts(req.MaxAttempts)
	timeoutMS := s.timeoutMS(req.TimeoutMS)

	endpoint, err := s.endpoints.CreateEndpoint(ctx, repository.CreateEndpointDraft{
		URL:              req.URL,
		Description:      req.Description,
		EventTypes:       eventTypes,
		Status:           dto.StatusActive,
		MaxAttempts:      maxAttempts,
		TimeoutMS:        timeoutMS,
		SecretCiphertext: ciphertext,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to create webhook endpoint")
	}

	s.recordAudit(ctx, actorID, "webhook.endpoint.created", "webhook_endpoint", endpoint.ID, map[string]any{
		"url":         endpoint.Url,
		"eventTypes":  endpoint.EventTypes,
		"status":      endpoint.Status,
		"maxAttempts": endpoint.MaxAttempts,
		"timeoutMs":   endpoint.TimeoutMs,
	})
	resp := toEndpointResponse(endpoint)
	return &dto.EndpointSecretResponse{EndpointResponse: resp, Secret: secret}, nil
}

func (s *service) UpdateEndpoint(ctx context.Context, actorID, id int64, req dto.UpdateEndpointRequest) (*dto.EndpointResponse, error) {
	if err := s.validateURL(req.URL); err != nil {
		return nil, err
	}
	eventTypes, err := normalizeEventTypes(req.EventTypes)
	if err != nil {
		return nil, err
	}
	if !validStatus(req.Status) {
		return nil, apperror.NewBadRequest("status must be one of: active, disabled")
	}
	endpoint, err := s.endpoints.UpdateEndpoint(ctx, repository.UpdateEndpointDraft{
		ID:          id,
		URL:         req.URL,
		Description: req.Description,
		EventTypes:  eventTypes,
		Status:      req.Status,
		MaxAttempts: s.maxAttempts(req.MaxAttempts),
		TimeoutMS:   s.timeoutMS(req.TimeoutMS),
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to update webhook endpoint")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.updated", "webhook_endpoint", id, map[string]any{
		"url":         endpoint.Url,
		"eventTypes":  endpoint.EventTypes,
		"status":      endpoint.Status,
		"maxAttempts": endpoint.MaxAttempts,
		"timeoutMs":   endpoint.TimeoutMs,
	})
	resp := toEndpointResponse(endpoint)
	return &resp, nil
}

func (s *service) DeleteEndpoint(ctx context.Context, actorID, id int64) error {
	if _, err := s.endpoints.DeleteEndpoint(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("webhook endpoint not found")
		}
		return apperror.NewInternal("failed to delete webhook endpoint")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.deleted", "webhook_endpoint", id, nil)
	return nil
}

func (s *service) GetEndpoint(ctx context.Context, id int64) (*dto.EndpointResponse, error) {
	endpoint, err := s.endpoints.GetEndpoint(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to get webhook endpoint")
	}
	resp := toEndpointResponse(endpoint)
	return &resp, nil
}

func (s *service) ListEndpoints(ctx context.Context, limit int, startingAfter string) ([]dto.EndpointResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	endpoints, err := s.endpoints.ListEndpoints(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list webhook endpoints")
	}
	hasMore := len(endpoints) > pageSize
	if hasMore {
		endpoints = endpoints[:pageSize]
	}
	out := make([]dto.EndpointResponse, len(endpoints))
	for i := range endpoints {
		out[i] = toEndpointResponse(&endpoints[i])
	}
	return out, hasMore, nil
}

func (s *service) RotateEndpointSecret(ctx context.Context, actorID, id int64) (*dto.EndpointSecretResponse, error) {
	secret, err := GenerateSecret(secretPrefixEndpoint)
	if err != nil {
		return nil, apperror.NewInternal("failed to generate webhook secret")
	}
	ciphertext, err := s.codec.Seal([]byte(secret))
	if err != nil {
		return nil, apperror.NewInternal("failed to protect webhook secret")
	}
	endpoint, err := s.endpoints.RotateEndpointSecret(ctx, id, ciphertext)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to rotate webhook secret")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.secret_rotated", "webhook_endpoint", id, nil)
	resp := toEndpointResponse(endpoint)
	return &dto.EndpointSecretResponse{EndpointResponse: resp, Secret: secret}, nil
}

func (s *service) ListDeliveries(ctx context.Context, endpointID int64, status string, limit int, startingAfter string) ([]dto.DeliveryResponse, bool, error) {
	if status != "" && status != dto.DeliveryPending && status != dto.DeliverySucceeded && status != dto.DeliveryDead {
		return nil, false, apperror.NewBadRequest("status must be one of: pending, succeeded, dead")
	}
	if endpointID < 0 {
		return nil, false, apperror.NewBadRequest("invalid endpointId")
	}
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	deliveries, err := s.deliveries.ListDeliveries(ctx, repository.ListDeliveriesParams{
		EndpointID: endpointID,
		Status:     status,
		Cursor:     cur,
	})
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list webhook deliveries")
	}
	hasMore := len(deliveries) > pageSize
	if hasMore {
		deliveries = deliveries[:pageSize]
	}
	out := make([]dto.DeliveryResponse, len(deliveries))
	for i := range deliveries {
		out[i] = toDeliveryResponse(&deliveries[i], false)
	}
	return out, hasMore, nil
}

func (s *service) GetDelivery(ctx context.Context, id int64) (*dto.DeliveryResponse, error) {
	delivery, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	resp := toDeliveryResponse(delivery, true)
	return &resp, nil
}

func (s *service) ListDeliveryAttempts(ctx context.Context, id int64) ([]dto.DeliveryAttemptResponse, error) {
	if _, err := s.deliveries.GetDelivery(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	attempts, err := s.deliveries.ListDeliveryAttempts(ctx, id)
	if err != nil {
		return nil, apperror.NewInternal("failed to list webhook delivery attempts")
	}
	out := make([]dto.DeliveryAttemptResponse, len(attempts))
	for i := range attempts {
		out[i] = toAttemptResponse(&attempts[i])
	}
	return out, nil
}

func (s *service) ReplayDelivery(ctx context.Context, actorID, id int64) (*dto.DeliveryResponse, error) {
	original, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	endpoint, err := s.endpoints.GetEndpoint(ctx, original.EndpointID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to get webhook endpoint")
	}
	if endpoint.Status != dto.StatusActive {
		return nil, apperror.NewConflict("webhook endpoint is disabled")
	}
	ciphertext, err := s.endpoints.ActiveEndpointSecret(ctx, endpoint.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewConflict("webhook endpoint has no active secret")
		}
		return nil, apperror.NewInternal("failed to get webhook secret")
	}

	eventID := fmt.Sprintf("replay.%d.%s", original.ID, randomHex(8))
	newID, created, err := s.deliveries.CreateDeliveryWithJob(ctx, repository.DeliveryDraft{
		TenantID:         tenant.EffectiveID(ctx),
		EndpointID:       endpoint.ID,
		EventID:          eventID,
		EventType:        original.EventType,
		EventCreatedAt:   s.now(),
		URL:              endpoint.Url,
		Payload:          json.RawMessage(original.Payload),
		SecretCiphertext: ciphertext,
		MaxAttempts:      original.MaxAttempts,
		RunAt:            s.now(),
		ReplayedFromID:   original.ID,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to replay webhook delivery")
	}
	if !created {
		return nil, apperror.NewConflict("webhook delivery replay already exists")
	}
	delivery, err := s.deliveries.GetDelivery(ctx, newID)
	if err != nil {
		return nil, apperror.NewInternal("failed to load replayed webhook delivery")
	}
	s.recordAudit(ctx, actorID, "webhook.delivery.replayed", "webhook_delivery", newID, map[string]any{
		"originalDeliveryId": original.ID,
		"endpointId":         endpoint.ID,
		"eventType":          original.EventType,
	})
	resp := toDeliveryResponse(delivery, true)
	return &resp, nil
}

func (s *service) CreateInboundSource(ctx context.Context, actorID int64, req dto.CreateInboundSourceRequest) (*dto.InboundSourceSecretResponse, error) {
	if err := validateSlug(req.Slug); err != nil {
		return nil, err
	}
	secret, err := GenerateSecret(secretPrefixInbound)
	if err != nil {
		return nil, apperror.NewInternal("failed to generate inbound secret")
	}
	ciphertext, err := s.codec.Seal([]byte(secret))
	if err != nil {
		return nil, apperror.NewInternal("failed to protect inbound secret")
	}
	source, err := s.inbound.CreateInboundSource(ctx, repository.InboundSourceDraft{
		Name:             strings.TrimSpace(req.Name),
		Slug:             req.Slug,
		SecretCiphertext: ciphertext,
	})
	if err != nil {
		if isConflict(err) {
			return nil, err
		}
		return nil, apperror.NewInternal("failed to create inbound source")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.created", "webhook_inbound_source", source.ID, map[string]any{
		"name": source.Name,
		"slug": source.Slug,
	})
	resp := toInboundSourceResponse(source)
	return &dto.InboundSourceSecretResponse{InboundSourceResponse: resp, Secret: secret}, nil
}

func (s *service) UpdateInboundSource(ctx context.Context, actorID, id int64, req dto.UpdateInboundSourceRequest) (*dto.InboundSourceResponse, error) {
	if err := validateSlug(req.Slug); err != nil {
		return nil, err
	}
	if !validStatus(req.Status) {
		return nil, apperror.NewBadRequest("status must be one of: active, disabled")
	}
	source, err := s.inbound.UpdateInboundSource(ctx, repository.UpdateInboundSourceDraft{
		ID:     id,
		Name:   strings.TrimSpace(req.Name),
		Slug:   req.Slug,
		Status: req.Status,
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		if isConflict(err) {
			return nil, err
		}
		return nil, apperror.NewInternal("failed to update inbound source")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.updated", "webhook_inbound_source", id, map[string]any{
		"name":   source.Name,
		"slug":   source.Slug,
		"status": source.Status,
	})
	resp := toInboundSourceResponse(source)
	return &resp, nil
}

func (s *service) DeleteInboundSource(ctx context.Context, actorID, id int64) error {
	if _, err := s.inbound.DeleteInboundSource(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("inbound source not found")
		}
		return apperror.NewInternal("failed to delete inbound source")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.deleted", "webhook_inbound_source", id, nil)
	return nil
}

func (s *service) GetInboundSource(ctx context.Context, id int64) (*dto.InboundSourceResponse, error) {
	source, err := s.inbound.GetInboundSource(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		return nil, apperror.NewInternal("failed to get inbound source")
	}
	resp := toInboundSourceResponse(source)
	return &resp, nil
}

func (s *service) ListInboundSources(ctx context.Context, limit int, startingAfter string) ([]dto.InboundSourceResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	sources, err := s.inbound.ListInboundSources(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list inbound sources")
	}
	hasMore := len(sources) > pageSize
	if hasMore {
		sources = sources[:pageSize]
	}
	out := make([]dto.InboundSourceResponse, len(sources))
	for i := range sources {
		out[i] = toInboundSourceResponse(&sources[i])
	}
	return out, hasMore, nil
}

func (s *service) RotateInboundSourceSecret(ctx context.Context, actorID, id int64) (*dto.InboundSourceSecretResponse, error) {
	secret, err := GenerateSecret(secretPrefixInbound)
	if err != nil {
		return nil, apperror.NewInternal("failed to generate inbound secret")
	}
	ciphertext, err := s.codec.Seal([]byte(secret))
	if err != nil {
		return nil, apperror.NewInternal("failed to protect inbound secret")
	}
	source, err := s.inbound.RotateInboundSourceSecret(ctx, id, ciphertext)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		return nil, apperror.NewInternal("failed to rotate inbound secret")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.secret_rotated", "webhook_inbound_source", id, nil)
	resp := toInboundSourceResponse(source)
	return &dto.InboundSourceSecretResponse{InboundSourceResponse: resp, Secret: secret}, nil
}

func (s *service) ListInboundEvents(ctx context.Context, sourceID int64, limit int, startingAfter string) ([]dto.InboundEventResponse, bool, error) {
	if _, err := s.inbound.GetInboundSource(ctx, sourceID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, false, apperror.NewNotFound("inbound source not found")
		}
		return nil, false, apperror.NewInternal("failed to get inbound source")
	}
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	events, err := s.inbound.ListInboundEvents(ctx, sourceID, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list inbound events")
	}
	hasMore := len(events) > pageSize
	if hasMore {
		events = events[:pageSize]
	}
	out := make([]dto.InboundEventResponse, len(events))
	for i := range events {
		out[i] = toInboundEventResponse(&events[i])
	}
	return out, hasMore, nil
}

// ReceiveInbound authenticates a public inbound request. The source ID in the
// URL identifies the tenant, so X-Tenant-Slug is deliberately ignored. Callers
// receive the owning tenant in the receipt so the HTTP layer can align Fibers
// locals (and therefore the audit middleware) with the source tenant.
func (s *service) ReceiveInbound(ctx context.Context, sourceID int64, headers http.Header, body []byte) (*dto.InboundReceipt, error) {
	if len(body) == 0 {
		return nil, apperror.NewBadRequest("request body is required")
	}
	if len(body) > maxInboundPayloadBytes {
		return nil, apperror.NewBadRequest("inbound payload exceeds 1MB")
	}
	if !json.Valid(body) {
		return nil, apperror.NewBadRequest("payload must be valid JSON")
	}

	source, err := s.inbound.GetInboundSourceByID(ctx, sourceID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		return nil, apperror.NewInternal("failed to resolve inbound source")
	}
	if source.Status != dto.StatusActive {
		return nil, apperror.NewNotFound("inbound source not found")
	}

	secret, err := s.codec.Open(source.SecretCiphertext)
	if err != nil {
		slog.Error("webhooks: failed to open inbound source secret",
			slog.Int64("source_id", source.ID),
			slog.Any("error", err),
		)
		return nil, apperror.NewInternal("failed to verify inbound signature")
	}
	if err := webhook.VerifyRequest(secret, headers, body, s.opts.TimestampTolerance, s.now()); err != nil {
		switch {
		case errors.Is(err, webhook.ErrMissingSignature), errors.Is(err, webhook.ErrMissingTimestamp):
			return nil, apperror.NewUnauthorized("webhook signature is required")
		case errors.Is(err, webhook.ErrTimestampOutOfRange), errors.Is(err, webhook.ErrInvalidTimestamp):
			return nil, apperror.NewUnauthorized("webhook timestamp is invalid or expired")
		default:
			return nil, apperror.NewUnauthorized("webhook signature is invalid")
		}
	}

	externalID := strings.TrimSpace(headers.Get(webhook.HeaderID))
	if externalID == "" {
		return nil, apperror.NewBadRequest("X-Webhook-Id is required")
	}
	if len(externalID) > maxEventIDLen || hasControl(externalID) {
		return nil, apperror.NewBadRequest("X-Webhook-Id is invalid")
	}
	eventType := strings.TrimSpace(headers.Get(webhook.HeaderEvent))
	if eventType == "" {
		eventType = "inbound"
	}
	if !webhookcontracts.ValidType(eventType) || strings.Contains(eventType, "*") {
		return nil, apperror.NewBadRequest("X-Webhook-Event is invalid")
	}

	ctx = tenant.WithID(ctx, source.TenantID)
	signature := headers.Get(webhook.HeaderSignature)
	if len(signature) > 255 {
		signature = signature[:255]
	}
	eventID, created, err := s.inbound.CreateInboundEvent(ctx, repository.InboundEventDraft{
		SourceID:        source.ID,
		ExternalEventID: externalID,
		EventType:       eventType,
		Signature:       signature,
		Payload:         json.RawMessage(body),
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to store inbound event")
	}

	receipt := &dto.InboundReceipt{
		ID:         eventID,
		TenantID:   source.TenantID,
		SourceID:   source.ID,
		EventID:    externalID,
		EventType:  eventType,
		ReceivedAt: s.now(),
		Duplicate:  !created,
	}
	if !created {
		existing, getErr := s.inbound.GetInboundEventByExternalID(ctx, source.ID, externalID)
		if getErr != nil {
			return nil, apperror.NewInternal("failed to load duplicate inbound event")
		}
		receipt.ID = existing.ID
		receipt.EventType = existing.EventType
		receipt.ReceivedAt = existing.ReceivedAt.Time.UTC()
	}

	s.recordAudit(ctx, 0, "webhook.inbound.received", "webhook_inbound_event", receipt.ID, map[string]any{
		"sourceId":  source.ID,
		"eventId":   externalID,
		"type":      receipt.EventType,
		"duplicate": receipt.Duplicate,
	})
	return receipt, nil
}

func (s *service) PurgeRetention(ctx context.Context, retentionDays int32) (deliveries, events int64, err error) {
	deliveries, err = s.deliveries.PurgeDeliveries(ctx, retentionDays)
	if err != nil {
		return 0, 0, err
	}
	events, err = s.inbound.PurgeInboundEvents(ctx, retentionDays)
	if err != nil {
		return deliveries, 0, err
	}
	return deliveries, events, nil
}

func (s *service) validateURL(raw string) error {
	if len(raw) > 2048 {
		return apperror.NewBadRequest("url must be at most 2048 characters")
	}
	if _, err := s.urlGuard.ValidateURL(raw); err != nil {
		return apperror.NewBadRequest("url must be an absolute http(s) URL")
	}
	parsed, _ := url.Parse(raw)
	if parsed.Hostname() == "" {
		return apperror.NewBadRequest("url must include a host")
	}
	return nil
}

func (s *service) maxAttempts(value *int) int32 {
	if value == nil {
		return int32(s.opts.DefaultMaxAttempts) // #nosec G115 -- Option bounds are 1..50
	}
	if *value < 1 || *value > 50 {
		return int32(s.opts.DefaultMaxAttempts) // #nosec G115
	}
	return int32(*value) // #nosec G115 -- validated 1..50
}

func (s *service) timeoutMS(value *int) int32 {
	if value == nil {
		return s.opts.DefaultTimeoutMS
	}
	if *value < 100 || *value > 120000 {
		return s.opts.DefaultTimeoutMS
	}
	return int32(*value) // #nosec G115 -- validated 100..120000
}

func (s *service) recordAudit(ctx context.Context, actorID int64, action, resource string, resourceID int64, metadata map[string]any) {
	if s.audit == nil {
		return
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	event := audit.Event{
		TenantID:   tenant.EffectiveID(ctx),
		ActorID:    actorID,
		Action:     action,
		Resource:   resource,
		ResourceID: fmt.Sprintf("%d", resourceID),
		Metadata:   metadata,
		CreatedAt:  s.now(),
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := s.audit.Record(auditCtx, event); err != nil {
		slog.Warn("webhooks: failed to record audit event",
			slog.String("action", action),
			slog.Any("error", err),
		)
	}
}

func normalizeEventTypes(types []string) ([]string, error) {
	if len(types) == 0 {
		return []string{"*"}, nil
	}
	if len(types) > 20 {
		return nil, apperror.NewBadRequest("eventTypes must contain at most 20 entries")
	}
	out := make([]string, 0, len(types))
	seen := make(map[string]struct{}, len(types))
	for _, raw := range types {
		pattern := strings.TrimSpace(raw)
		if !webhookcontracts.ValidType(pattern) {
			return nil, apperror.NewBadRequest("invalid event type pattern: " + raw)
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	if len(out) == 0 {
		return nil, apperror.NewBadRequest("eventTypes must not be empty")
	}
	return out, nil
}

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return apperror.NewBadRequest("slug must match [a-z0-9-]{1,63}")
	}
	return nil
}

func validStatus(status string) bool {
	return status == dto.StatusActive || status == dto.StatusDisabled
}

func buildCursor(limit int, startingAfter string) (pagination.DBCursor, int, error) {
	pageSize := pagination.NormalizeLimit(limit)
	cur := pagination.DBCursor{Limit: pagination.RowLimit(pageSize + 1)}
	if startingAfter != "" {
		ts, id, err := pagination.DecodeCursor(startingAfter)
		if err != nil {
			return pagination.DBCursor{}, 0, apperror.NewBadRequest("invalid cursor")
		}
		cur.HasCursor = true
		cur.CreatedAt = ts
		cur.ID = id
	}
	return cur, pageSize, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable for delivery identity; fall back
		// to a time-based suffix rather than panicking inside a request.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isConflict(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && appErr.Status == http.StatusConflict
}

func toEndpointResponse(endpoint *webhooksqlc.WebhookEndpoint) dto.EndpointResponse {
	resp := dto.EndpointResponse{
		ID:          endpoint.ID,
		TenantID:    endpoint.TenantID,
		URL:         endpoint.Url,
		Description: endpoint.Description,
		EventTypes:  endpoint.EventTypes,
		Status:      endpoint.Status,
		MaxAttempts: endpoint.MaxAttempts,
		TimeoutMS:   endpoint.TimeoutMs,
		CreatedAt:   endpoint.CreatedAt.Time.UTC(),
		UpdatedAt:   endpoint.UpdatedAt.Time.UTC(),
	}
	if resp.EventTypes == nil {
		resp.EventTypes = []string{}
	}
	if endpoint.DeletedAt.Valid {
		deletedAt := endpoint.DeletedAt.Time.UTC()
		resp.DeletedAt = &deletedAt
	}
	return resp
}

func toDeliveryResponse(delivery *webhooksqlc.WebhookDelivery, includePayload bool) dto.DeliveryResponse {
	resp := dto.DeliveryResponse{
		ID:            delivery.ID,
		TenantID:      delivery.TenantID,
		EndpointID:    delivery.EndpointID,
		EventID:       delivery.EventID,
		EventType:     delivery.EventType,
		Status:        delivery.Status,
		Attempts:      delivery.Attempts,
		MaxAttempts:   delivery.MaxAttempts,
		NextAttemptAt: delivery.NextAttemptAt.Time.UTC(),
		CreatedAt:     delivery.CreatedAt.Time.UTC(),
		UpdatedAt:     delivery.UpdatedAt.Time.UTC(),
	}
	if delivery.LastStatusCode.Valid {
		code := int(delivery.LastStatusCode.Int32)
		resp.LastStatusCode = &code
	}
	if delivery.LastError.Valid {
		resp.LastError = delivery.LastError.String
	}
	if delivery.DeliveredAt.Valid {
		at := delivery.DeliveredAt.Time.UTC()
		resp.DeliveredAt = &at
	}
	if delivery.ReplayedFromID.Valid {
		id := delivery.ReplayedFromID.Int64
		resp.ReplayedFromID = &id
	}
	if includePayload {
		resp.Payload = json.RawMessage(delivery.Payload)
	}
	return resp
}

func toAttemptResponse(attempt *webhooksqlc.WebhookDeliveryAttempt) dto.DeliveryAttemptResponse {
	resp := dto.DeliveryAttemptResponse{
		ID:         attempt.ID,
		DeliveryID: attempt.DeliveryID,
		Attempt:    attempt.Attempt,
		DurationMS: attempt.DurationMs,
		CreatedAt:  attempt.CreatedAt.Time.UTC(),
	}
	if attempt.StatusCode.Valid {
		code := int(attempt.StatusCode.Int32)
		resp.StatusCode = &code
	}
	if attempt.Error.Valid {
		resp.Error = attempt.Error.String
	}
	return resp
}

func toInboundSourceResponse(source *webhooksqlc.WebhookInboundSource) dto.InboundSourceResponse {
	resp := dto.InboundSourceResponse{
		ID:          source.ID,
		TenantID:    source.TenantID,
		Name:        source.Name,
		Slug:        source.Slug,
		Status:      source.Status,
		ReceiverURL: fmt.Sprintf("/api/v1/webhooks/inbound/%d", source.ID),
		CreatedAt:   source.CreatedAt.Time.UTC(),
		UpdatedAt:   source.UpdatedAt.Time.UTC(),
	}
	if source.DeletedAt.Valid {
		deletedAt := source.DeletedAt.Time.UTC()
		resp.DeletedAt = &deletedAt
	}
	return resp
}

func toInboundEventResponse(event *webhooksqlc.WebhookInboundEvent) dto.InboundEventResponse {
	return dto.InboundEventResponse{
		ID:              event.ID,
		TenantID:        event.TenantID,
		SourceID:        event.SourceID,
		ExternalEventID: event.ExternalEventID,
		EventType:       event.EventType,
		Payload:         json.RawMessage(event.Payload),
		ReceivedAt:      event.ReceivedAt.Time.UTC(),
	}
}
