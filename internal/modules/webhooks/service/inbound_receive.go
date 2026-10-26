package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const (
	// defaultInboundEventType is used when the signed body has no "type" member.
	defaultInboundEventType = "inbound"
	// maxStoredSignatureLen bounds the signature header kept for forensics.
	maxStoredSignatureLen = 255
)

// ReceiveInbound authenticates a public inbound request. The source ID in the
// URL identifies the tenant, so X-Tenant-Slug is deliberately ignored. Callers
// receive the owning tenant in the receipt so the HTTP layer can align Fibers
// locals (and therefore the audit middleware) with the source tenant.
//
// The steps run in a fixed order: validate the body, resolve an active source
// of an active tenant, validate the event id, verify the signature, validate
// the event type, then store the event once per (source, webhook-id).
func (s *service) ReceiveInbound(ctx context.Context, sourceID int64, headers http.Header, body []byte) (*InboundReceipt, error) {
	if err := validateInboundBody(body); err != nil {
		return nil, err
	}
	source, err := s.activeInboundSource(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	// The id is part of the signed content, so it is validated before
	// verification and used verbatim afterwards.
	externalID, err := inboundEventID(headers)
	if err != nil {
		return nil, err
	}
	if err := s.verifyInboundSignature(source, headers, body); err != nil {
		return nil, err
	}
	eventType, err := inboundEventType(body)
	if err != nil {
		return nil, err
	}

	ctx = tenant.WithID(ctx, source.TenantID)
	receipt, err := s.storeInboundEvent(ctx, source, externalID, eventType, headers, body)
	if err != nil {
		return nil, err
	}
	s.recordAudit(ctx, 0, "webhook.inbound.received", "webhook_inbound_event", receipt.ID, map[string]any{
		"sourceId":  source.ID,
		"eventId":   externalID,
		"type":      receipt.EventType,
		"duplicate": receipt.Duplicate,
	})
	return receipt, nil
}

func validateInboundBody(body []byte) error {
	if len(body) == 0 {
		return apperror.NewBadRequest("request body is required")
	}
	if len(body) > maxInboundPayloadBytes {
		return apperror.NewBadRequest("inbound payload exceeds 1MB")
	}
	if !json.Valid(body) {
		return apperror.NewBadRequest("payload must be valid JSON")
	}
	return nil
}

// activeInboundSource resolves the source across tenants. A disabled source
// and a suspended tenant are indistinguishable from a missing source, like
// every other request for that tenant.
func (s *service) activeInboundSource(ctx context.Context, sourceID int64) (*InboundSource, error) {
	source, err := s.inbound.GetInboundSourceByID(ctx, sourceID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		return nil, apperror.NewInternal("failed to resolve inbound source")
	}
	if source.Status != StatusActive {
		return nil, apperror.NewNotFound("inbound source not found")
	}
	if s.opts.Tenants == nil {
		return source, nil
	}
	active, err := s.opts.Tenants.TenantActive(ctx, source.TenantID)
	if err != nil {
		slog.Error("webhooks: failed to check inbound source tenant",
			slog.Int64("source_id", source.ID),
			slog.Any("error", err),
		)
		return nil, apperror.NewInternal("failed to resolve inbound source")
	}
	if !active {
		return nil, apperror.NewNotFound("inbound source not found")
	}
	return source, nil
}

func inboundEventID(headers http.Header) (string, error) {
	externalID := headers.Get(webhook.HeaderID)
	trimmed := strings.TrimSpace(externalID)
	if trimmed == "" {
		return "", apperror.NewBadRequest("webhook-id is required")
	}
	if externalID != trimmed || len(externalID) > maxEventIDLen || hasControl(externalID) {
		return "", apperror.NewBadRequest("webhook-id is invalid")
	}
	return externalID, nil
}

// verifyInboundSignature checks the Standard Webhooks signature and timestamp
// with the source's current secret and, while its explicit grace window is
// open, the immediately previous secret. A sender that has not switched yet
// therefore keeps working for 24h after a rotation. Each failure class maps to
// a public 401 detail.
func (s *service) verifyInboundSignature(source *InboundSource, headers http.Header, body []byte) error {
	key, err := s.openInboundKey(source.SecretCiphertext, source.ID)
	if err != nil {
		return err
	}
	verifyErr := webhook.VerifyRequest(key, headers, body, s.opts.TimestampTolerance, s.now())
	if verifyErr == nil {
		return nil
	}
	// Only a signature mismatch can be rescued by the previous secret; a
	// missing or stale timestamp is the sender's mistake either way.
	if errors.Is(verifyErr, webhook.ErrInvalidSignature) && source.previousSecretValid(s.now()) {
		previousKey, err := s.openInboundKey(source.PreviousSecretCiphertext, source.ID)
		if err != nil {
			return verifyErr
		}
		if previousErr := webhook.VerifyRequest(previousKey, headers, body, s.opts.TimestampTolerance, s.now()); previousErr == nil {
			return nil
		}
	}
	return inboundVerifyError(verifyErr)
}

// openInboundKey opens and decodes one sealed source secret. A source created
// before Standard Webhooks has a secret no standard sender can sign with; the
// sender only learns the signature failed.
func (s *service) openInboundKey(ciphertext string, sourceID int64) ([]byte, error) {
	secret, err := s.codec.Open(ciphertext)
	if err != nil {
		slog.Error("webhooks: failed to open inbound source secret",
			slog.Int64("source_id", sourceID),
			slog.Any("error", err),
		)
		return nil, apperror.NewInternal("failed to verify inbound signature")
	}
	key, err := webhook.DecodeSecret(string(secret))
	if err != nil {
		slog.Error("webhooks: inbound source secret predates Standard Webhooks; rotate it",
			slog.Int64("source_id", sourceID),
			slog.Any("error", err),
		)
		return nil, apperror.NewUnauthorized("webhook signature is invalid")
	}
	return key, nil
}

func (s *InboundSource) previousSecretValid(now time.Time) bool {
	return s.PreviousSecretCiphertext != "" &&
		s.PreviousSecretExpiresAt != nil &&
		now.Before(*s.PreviousSecretExpiresAt)
}

func inboundVerifyError(err error) error {
	switch {
	case errors.Is(err, webhook.ErrMissingSignature), errors.Is(err, webhook.ErrMissingTimestamp), errors.Is(err, webhook.ErrMissingID):
		return apperror.NewUnauthorized("webhook signature is required")
	case errors.Is(err, webhook.ErrTimestampOutOfRange), errors.Is(err, webhook.ErrInvalidTimestamp):
		return apperror.NewUnauthorized("webhook timestamp is invalid or expired")
	default:
		return apperror.NewUnauthorized("webhook signature is invalid")
	}
}

// inboundEventType reads the event type from the verified body: Standard
// Webhooks payloads carry it as a top-level "type" member, not a header. A
// body that is not an object, or has no type, is stored as
// defaultInboundEventType; a type that is present but not a concrete event
// name is rejected.
func inboundEventType(body []byte) (string, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		return defaultInboundEventType, nil
	}
	raw, ok := members["type"]
	if !ok {
		return defaultInboundEventType, nil
	}
	var eventType string
	if err := json.Unmarshal(raw, &eventType); err != nil || !ValidType(eventType) || strings.Contains(eventType, "*") {
		return "", apperror.NewBadRequest("payload type is invalid")
	}
	return eventType, nil
}

// storeInboundEvent records the event once per (source, external id). A
// duplicate returns the receipt of the event that was stored first.
func (s *service) storeInboundEvent(ctx context.Context, source *InboundSource, externalID, eventType string, headers http.Header, body []byte) (*InboundReceipt, error) {
	signature := headers.Get(webhook.HeaderSignature)
	if len(signature) > maxStoredSignatureLen {
		signature = signature[:maxStoredSignatureLen]
	}
	eventID, created, err := s.inbound.CreateInboundEvent(ctx, InboundEventDraft{
		SourceID:        source.ID,
		ExternalEventID: externalID,
		EventType:       eventType,
		Signature:       signature,
		Payload:         json.RawMessage(body),
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to store inbound event")
	}

	receipt := &InboundReceipt{
		ID:         eventID,
		TenantID:   source.TenantID,
		SourceID:   source.ID,
		EventID:    externalID,
		EventType:  eventType,
		ReceivedAt: s.now(),
		Duplicate:  !created,
	}
	if created {
		return receipt, nil
	}
	existing, err := s.inbound.GetInboundEventByExternalID(ctx, source.ID, externalID)
	if err != nil {
		return nil, apperror.NewInternal("failed to load duplicate inbound event")
	}
	receipt.ID = existing.ID
	receipt.EventType = existing.EventType
	receipt.ReceivedAt = existing.ReceivedAt.UTC()
	return receipt, nil
}

// ListInboundEvents pages through the events received by one source.
func (s *service) ListInboundEvents(ctx context.Context, sourceID int64, limit int, startingAfter string) ([]InboundEvent, bool, error) {
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
	return events, hasMore, nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
