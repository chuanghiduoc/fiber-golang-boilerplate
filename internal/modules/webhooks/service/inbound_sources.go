package service

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

func (s *service) CreateInboundSource(ctx context.Context, actorID int64, in CreateInboundSourceInput) (*InboundSource, string, error) {
	if err := validateSlug(in.Slug); err != nil {
		return nil, "", err
	}
	secret, err := newSecret(inboundSecretLabel)
	if err != nil {
		return nil, "", err
	}
	tenantID := tenant.EffectiveID(ctx)
	source, err := s.inbound.CreateInboundSource(ctx, InboundSourceDraft{
		Name: strings.TrimSpace(in.Name),
		Slug: in.Slug,
		SealSecret: func(sourceID int64) (string, error) {
			return s.sealSecret(inboundSecretLabel, InboundSourceSecretBinding(tenantID, sourceID), secret)
		},
	})
	if err != nil {
		if isConflict(err) {
			return nil, "", err
		}
		return nil, "", apperror.NewInternal("failed to create inbound source")
	}
	s.recordAudit(ctx, actorID, "webhook.inbound_source.created", "webhook_inbound_source", source.ID, map[string]any{
		"name": source.Name,
		"slug": source.Slug,
	})
	return source, secret, nil
}

func (s *service) UpdateInboundSource(ctx context.Context, actorID, id int64, in UpdateInboundSourceInput) (*InboundSource, error) {
	if err := validateSlug(in.Slug); err != nil {
		return nil, err
	}
	if !validStatus(in.Status) {
		return nil, apperror.NewBadRequest("status must be one of: active, disabled")
	}
	source, err := s.inbound.UpdateInboundSource(ctx, UpdateInboundSourceDraft{
		ID:     id,
		Name:   strings.TrimSpace(in.Name),
		Slug:   in.Slug,
		Status: in.Status,
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
	return source, nil
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

func (s *service) GetInboundSource(ctx context.Context, id int64) (*InboundSource, error) {
	source, err := s.inbound.GetInboundSource(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("inbound source not found")
		}
		return nil, apperror.NewInternal("failed to get inbound source")
	}
	return source, nil
}

func (s *service) ListInboundSources(ctx context.Context, limit int, startingAfter string) ([]InboundSource, bool, error) {
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
	return sources, hasMore, nil
}

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return apperror.NewBadRequest("slug must match [a-z0-9-]{1,63}")
	}
	return nil
}
