package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

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

// errInboundSourceUnavailable covers a missing, disabled, soft-deleted, or
// suspended-tenant source. The receiver treats all of them like a bad
// signature so sequential ids cannot be enumerated.
var errInboundSourceUnavailable = errors.New("webhooks: inbound source is unavailable")

// dummyInboundKey is a valid Standard Webhooks key used to run the HMAC for an
// unavailable source. Its result is discarded; the work keeps the unknown-id
// and bad-signature paths comparable in cost.
var dummyInboundKey = func() []byte {
	key, err := webhook.DecodeSecret(webhook.SecretPrefix + strings.Repeat("A", 43) + "=")
	if err != nil {
		panic("webhooks: invalid built-in dummy inbound key: " + err.Error())
	}
	return key
}()

// ReceiveInbound authenticates a public inbound request. The source ID in the
// URL identifies the tenant, so X-Tenant-Slug is deliberately ignored. Callers
// receive the owning tenant in the receipt so the HTTP layer can align Fibers
// locals (and therefore the audit middleware) with the source tenant.
//
// The steps run in a fixed order: validate the body and event id, resolve an
// active source of an active tenant, verify the signature, validate the event
// type, then store the event once per (source, webhook-id). An unknown,
// disabled, or deleted source returns the same 401 as a bad signature, so the
// endpoint does not reveal which sequential ids exist.
func (s *service) ReceiveInbound(ctx context.Context, sourceID uuid.UUID, headers http.Header, body []byte) (*InboundReceipt, error) {
	if err := validateInboundBody(body); err != nil {
		return nil, err
	}
	// The id is part of the signed content, so it is validated before
	// verification and used verbatim afterwards.
	externalID, err := inboundEventID(headers)
	if err != nil {
		return nil, err
	}
	source, tenantChecked, err := s.activeInboundSource(ctx, sourceID)
	if err != nil {
		if errors.Is(err, errInboundSourceUnavailable) {
			s.performDummyInboundWork(ctx, sourceID, source, tenantChecked, headers, body)
			return nil, inboundAuthError()
		}
		return nil, err
	}
	// The source owns the tenant. Install it before reading the source's
	// versioned secret rows so every lookup stays tenant-scoped.
	ctx = tenant.WithID(ctx, source.TenantID)
	if err := s.verifyInboundSignature(ctx, source, headers, body); err != nil {
		return nil, err
	}
	eventType, err := inboundEventType(body)
	if err != nil {
		return nil, err
	}

	receipt, err := s.storeInboundEvent(ctx, source, externalID, eventType, headers, body)
	if err != nil {
		return nil, err
	}
	if _, audited := s.inbound.(auditedInboundStore); !audited {
		s.recordAudit(ctx, uuid.Nil, "webhook.received", "webhook_inbound_event", receipt.ID, map[string]any{
			"sourceId":  source.ID,
			"eventId":   externalID,
			"type":      receipt.EventType,
			"duplicate": receipt.Duplicate,
		})
	}
	return receipt, nil
}

// verifyWithDummySecret performs the same HMAC work as a real verification and
// discards the outcome.
func (s *service) verifyWithDummySecret(headers http.Header, body []byte) {
	_ = webhook.VerifyRequest(dummyInboundKey, headers, body, s.opts.TimestampTolerance, s.now())
}

// performDummyInboundWork spends roughly the same work on an unavailable source
// as on a real one: a tenant lookup when the real path did not get to it, an
// AES-GCM open of a valid envelope, and the HMAC verification. Without it,
// sequential source ids could be probed by response latency. tenantChecked
// reports whether activeInboundSource already ran the tenant lookup.
func (s *service) performDummyInboundWork(ctx context.Context, sourceID uuid.UUID, source *InboundSource, tenantChecked bool, headers http.Header, body []byte) {
	if s.opts.Tenants != nil && !tenantChecked {
		tenantID := sourceID
		if source != nil {
			tenantID = source.TenantID
		}
		if _, err := s.opts.Tenants.TenantActive(ctx, tenantID); err != nil {
			slog.Error("webhooks: failed to check inbound source tenant",
				slog.String("source_id", sourceID.String()),
				slog.Any("error", err),
			)
		}
	}
	if s.codec != nil && s.dummyInboundCiphertext != "" {
		// The binding is deliberately wrong; only the AES-GCM work matters.
		_, _ = s.codec.Open(InboundSourceSecretBinding(tenant.DefaultID, sourceID), s.dummyInboundCiphertext)
	}
	s.verifyWithDummySecret(headers, body)
}

// inboundAuthError is the single 401 used for every authentication failure of
// the public receiver, including unavailable sources.
func inboundAuthError() error {
	return apperror.NewUnauthorized("webhook signature is invalid")
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

// activeInboundSource resolves the source across tenants. A disabled source, a
// suspended tenant, and a database error while checking the tenant all return
// errInboundSourceUnavailable so the receiver can answer the same 401. The
// source is returned alongside the error when it was loaded, and tenantChecked
// reports whether the tenant lookup ran (so the caller can spend it only when
// the real path did not).
func (s *service) activeInboundSource(ctx context.Context, sourceID uuid.UUID) (*InboundSource, bool, error) {
	source, err := s.inbound.GetInboundSourceByID(ctx, sourceID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, false, errInboundSourceUnavailable
		}
		return nil, false, apperror.Internal(err, "failed to resolve inbound source")
	}
	if source.Status != StatusActive {
		return source, false, errInboundSourceUnavailable
	}
	if s.opts.Tenants == nil {
		return source, false, nil
	}
	active, err := s.opts.Tenants.TenantActive(ctx, source.TenantID)
	if err != nil {
		slog.Error("webhooks: failed to check inbound source tenant",
			slog.String("source_id", source.ID.String()),
			slog.Any("error", err),
		)
		return source, true, errInboundSourceUnavailable
	}
	if !active {
		return source, true, errInboundSourceUnavailable
	}
	return source, true, nil
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
// therefore keeps working for 24h after a rotation. Every failure maps to the
// same public 401, so a caller cannot tell which check failed.
func (s *service) verifyInboundSignature(ctx context.Context, source *InboundSource, headers http.Header, body []byte) error {
	binding := InboundSourceSecretBinding(source.TenantID, source.ID)
	secrets, err := s.inbound.InboundSigningSecrets(ctx, source.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			// An active source without a secret row cannot authenticate
			// anything; answer like a bad signature, never with a 500.
			return inboundAuthError()
		}
		slog.Error("webhooks: failed to load inbound source signing secrets",
			slog.String("source_id", source.ID.String()),
			slog.Any("error", err),
		)
		return apperror.Internal(err, "failed to verify inbound signature")
	}
	key, err := s.openInboundKey(binding, secrets.CurrentCiphertext, source.ID)
	if err != nil {
		return err
	}
	verifyErr := webhook.VerifyRequest(key, headers, body, s.opts.TimestampTolerance, s.now())
	if verifyErr == nil {
		return nil
	}
	// Only a signature mismatch can be rescued by the previous secret; a
	// missing or stale timestamp is the sender's mistake either way.
	if errors.Is(verifyErr, webhook.ErrInvalidSignature) && secrets.PreviousCiphertext != "" {
		previousKey, err := s.openInboundKey(binding, secrets.PreviousCiphertext, source.ID)
		if err != nil {
			slog.Error("failed to open an inbound source's previous secret", slog.String("source_id", source.ID.String()), slog.Any("error", err))
			return inboundVerifyError(verifyErr)
		}
		if previousErr := webhook.VerifyRequest(previousKey, headers, body, s.opts.TimestampTolerance, s.now()); previousErr == nil {
			return nil
		}
	}
	return inboundVerifyError(verifyErr)
}

// openInboundKey opens and decodes one sealed source secret. Every stored
// secret is a Standard Webhooks "whsec_" value; anything else is a structural
// error, and the sender only learns the signature failed.
func (s *service) openInboundKey(binding SecretBinding, ciphertext string, sourceID uuid.UUID) ([]byte, error) {
	secret, err := s.codec.Open(binding, ciphertext)
	if err != nil {
		slog.Error("webhooks: failed to open inbound source secret",
			slog.String("source_id", sourceID.String()),
			slog.Any("error", err),
		)
		return nil, apperror.Internal(err, "failed to verify inbound signature")
	}
	key, err := webhook.DecodeSecret(string(secret))
	if err != nil {
		slog.Error("webhooks: inbound source secret is not a Standard Webhooks secret",
			slog.String("source_id", sourceID.String()),
			slog.Any("error", err),
		)
		return nil, inboundAuthError()
	}
	return key, nil
}

// inboundVerifyError is the single public authentication failure for the
// receiver: missing, stale, or invalid signatures all look the same, so a
// caller cannot distinguish an unknown source from a bad signature.
func inboundVerifyError(error) error {
	return inboundAuthError()
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
	draft := InboundEventDraft{
		SourceID:        source.ID,
		ExternalEventID: externalID,
		EventType:       eventType,
		Signature:       signature,
		Payload:         json.RawMessage(body),
	}
	var (
		eventID uuid.UUID
		created bool
		err     error
	)
	if audited, ok := s.inbound.(auditedInboundStore); ok {
		eventID, created, err = audited.CreateInboundEventAudited(ctx, draft, source.TenantID, map[string]any{
			"sourceId": source.ID,
			"eventId":  externalID,
			"type":     eventType,
		})
	} else {
		eventID, created, err = s.inbound.CreateInboundEvent(ctx, draft)
	}
	if err != nil {
		return nil, apperror.Internal(err, "failed to store inbound event")
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
		return nil, apperror.Internal(err, "failed to load duplicate inbound event")
	}
	receipt.ID = existing.ID
	receipt.EventType = existing.EventType
	receipt.ReceivedAt = existing.ReceivedAt.UTC()
	return receipt, nil
}

// ListInboundEvents pages through the events received by one source.
func (s *service) ListInboundEvents(ctx context.Context, sourceID uuid.UUID, limit int, startingAfter string) ([]InboundEvent, bool, error) {
	if _, err := s.inbound.GetInboundSource(ctx, sourceID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, false, apperror.NewNotFound("inbound source not found")
		}
		return nil, false, apperror.Internal(err, "failed to get inbound source")
	}
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	events, err := s.inbound.ListInboundEvents(ctx, sourceID, cur)
	if err != nil {
		return nil, false, apperror.Internal(err, "failed to list inbound events")
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

// auditedInboundStore is the optional transactional audit extension the
// PostgreSQL store implements. A public receiver has no tenant middleware, so
// the event row and its Tier 1 audit row must commit together with the source
// tenant; test doubles keep the post-hoc recorder path.
type auditedInboundStore interface {
	CreateInboundEventAudited(
		ctx context.Context,
		draft InboundEventDraft,
		tenantID uuid.UUID,
		metadata map[string]any,
	) (uuid.UUID, bool, error)
}
