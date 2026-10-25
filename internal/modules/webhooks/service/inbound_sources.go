package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

func (s *service) CreateInboundSource(ctx context.Context, actorID int64, req dto.CreateInboundSourceRequest) (*dto.InboundSourceSecretResponse, error) {
	if err := validateSlug(req.Slug); err != nil {
		return nil, err
	}
	secret, ciphertext, err := s.newSealedSecret(inboundSecretLabel)
	if err != nil {
		return nil, err
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

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return apperror.NewBadRequest("slug must match [a-z0-9-]{1,63}")
	}
	return nil
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
