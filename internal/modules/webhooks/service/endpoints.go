package service

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// urlValidationTimeout bounds the DNS lookup done when an endpoint URL is
// created or updated. The dialer resolves and validates again before sending.
const urlValidationTimeout = 3 * time.Second

// CreateEndpoint validates configuration, seals a fresh signing secret, and
// returns the plaintext secret exactly once.
func (s *service) CreateEndpoint(ctx context.Context, actorID int64, in CreateEndpointInput) (*Endpoint, string, error) {
	if err := s.validateURL(ctx, in.URL); err != nil {
		return nil, "", err
	}
	eventTypes, err := normalizeEventTypes(in.EventTypes)
	if err != nil {
		return nil, "", err
	}
	secret, err := newSecret(endpointSecretLabel)
	if err != nil {
		return nil, "", err
	}
	tenantID := tenant.EffectiveID(ctx)
	endpoint, err := s.endpoints.CreateEndpoint(ctx, CreateEndpointDraft{
		URL:         in.URL,
		Description: in.Description,
		EventTypes:  eventTypes,
		Status:      StatusActive,
		MaxAttempts: s.maxAttempts(in.MaxAttempts),
		TimeoutMS:   s.timeoutMS(in.TimeoutMS),
		SealSecret: func(endpointID int64) (string, error) {
			return s.sealSecret(endpointSecretLabel, EndpointSecretBinding(tenantID, endpointID), secret)
		},
	})
	if err != nil {
		return nil, "", apperror.NewInternal("failed to create webhook endpoint")
	}

	s.recordAudit(ctx, actorID, "webhook.endpoint.created", "webhook_endpoint", endpoint.ID, endpointAuditMetadata(endpoint))
	return endpoint, secret, nil
}

func (s *service) UpdateEndpoint(ctx context.Context, actorID, id int64, in UpdateEndpointInput) (*Endpoint, error) {
	if err := s.validateURL(ctx, in.URL); err != nil {
		return nil, err
	}
	eventTypes, err := normalizeEventTypes(in.EventTypes)
	if err != nil {
		return nil, err
	}
	if !validStatus(in.Status) {
		return nil, apperror.NewBadRequest("status must be one of: active, disabled")
	}
	endpoint, err := s.endpoints.UpdateEndpoint(ctx, UpdateEndpointDraft{
		ID:          id,
		URL:         in.URL,
		Description: in.Description,
		EventTypes:  eventTypes,
		Status:      in.Status,
		MaxAttempts: s.maxAttempts(in.MaxAttempts),
		TimeoutMS:   s.timeoutMS(in.TimeoutMS),
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to update webhook endpoint")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.updated", "webhook_endpoint", id, endpointAuditMetadata(endpoint))
	return endpoint, nil
}

func (s *service) DeleteEndpoint(ctx context.Context, actorID, id int64) error {
	if err := s.endpoints.DeleteEndpoint(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("webhook endpoint not found")
		}
		return apperror.NewInternal("failed to delete webhook endpoint")
	}
	s.recordAudit(ctx, actorID, "webhook.endpoint.deleted", "webhook_endpoint", id, nil)
	return nil
}

func (s *service) GetEndpoint(ctx context.Context, id int64) (*Endpoint, error) {
	endpoint, err := s.endpoints.GetEndpoint(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to get webhook endpoint")
	}
	return endpoint, nil
}

func (s *service) ListEndpoints(ctx context.Context, limit int, startingAfter string) ([]Endpoint, bool, error) {
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
	return endpoints, hasMore, nil
}

func (s *service) validateURL(ctx context.Context, raw string) error {
	if len(raw) > 2048 {
		return apperror.NewBadRequest("url must be at most 2048 characters")
	}
	parsed, err := s.urlGuard.ValidateURL(raw)
	if err != nil {
		if errors.Is(err, webhook.ErrPortNotAllowed) {
			return apperror.NewCode(webhookscatalog.CodeWebhookPortNotAllowed,
				"url port is not allowed for webhook delivery")
		}
		return apperror.NewBadRequest("url must be an absolute http(s) URL")
	}
	if parsed.Hostname() == "" {
		return apperror.NewBadRequest("url must include a host")
	}
	if s.opts.RequireHTTPS && parsed.Scheme != "https" {
		return apperror.NewBadRequest("url must use https")
	}
	// Resolve now so a hostname pointing at a private/loopback address is
	// refused while the endpoint is configured, not after every delivery
	// fails. The guarded dialer still resolves and checks again (DNS
	// rebinding), so this is an early rejection, not the only check.
	resolveCtx, cancel := context.WithTimeout(ctx, urlValidationTimeout)
	defer cancel()
	if _, err := s.urlGuard.CheckHost(resolveCtx, parsed.Hostname()); err != nil {
		if errors.Is(err, webhook.ErrBlockedTarget) {
			return apperror.NewBadRequest("url host resolves to a blocked address")
		}
		return apperror.NewBadRequest("url host could not be resolved")
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

// normalizeEventTypes trims, validates, de-duplicates, and sorts the patterns.
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
		if !ValidType(pattern) {
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
	// Subscriptions are a set (webhook_endpoint_subscriptions); sorting here
	// makes the create/update response match every later read, which returns
	// them in the same byte order.
	sort.Strings(out)
	return out, nil
}

// endpointAuditMetadata is the non-secret configuration recorded when an
// endpoint is created or updated. The URL is reduced to its origin because
// receivers such as Slack embed bearer tokens in the path or query, and the
// audit log is append-only.
func endpointAuditMetadata(endpoint *Endpoint) map[string]any {
	return map[string]any{
		"url":         urlOrigin(endpoint.URL),
		"eventTypes":  endpoint.EventTypes,
		"status":      endpoint.Status,
		"maxAttempts": endpoint.MaxAttempts,
		"timeoutMs":   endpoint.TimeoutMS,
	}
}

// urlOrigin returns scheme://host[:port] and nothing else. Validation rejects
// malformed URLs, so an unparsable value is recorded as empty rather than
// risking a token in the audit log.
func urlOrigin(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
