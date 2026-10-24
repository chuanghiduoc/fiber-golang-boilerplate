package service

import (
	"context"
	"errors"
	"net/url"
	"strings"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

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
	secret, ciphertext, err := s.newSealedSecret(secretPrefixEndpoint, endpointSecretLabel)
	if err != nil {
		return nil, err
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

	s.recordAudit(ctx, actorID, "webhook.endpoint.created", "webhook_endpoint", endpoint.ID, endpointAuditMetadata(endpoint))
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
	s.recordAudit(ctx, actorID, "webhook.endpoint.updated", "webhook_endpoint", id, endpointAuditMetadata(endpoint))
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

// endpointAuditMetadata is the non-secret configuration recorded when an
// endpoint is created or updated.
func endpointAuditMetadata(endpoint *webhooksqlc.WebhookEndpoint) map[string]any {
	return map[string]any{
		"url":         endpoint.Url,
		"eventTypes":  endpoint.EventTypes,
		"status":      endpoint.Status,
		"maxAttempts": endpoint.MaxAttempts,
		"timeoutMs":   endpoint.TimeoutMs,
	}
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
