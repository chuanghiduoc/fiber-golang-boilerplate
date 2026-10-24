package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func (r *Postgres) CreateInboundSource(ctx context.Context, draft InboundSourceDraft) (*webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	source, err := r.q.CreateWebhookInboundSource(ctx, webhooksqlc.CreateWebhookInboundSourceParams{
		TenantID:         tenantID(ctx),
		Name:             draft.Name,
		Slug:             draft.Slug,
		SecretCiphertext: draft.SecretCiphertext,
	})
	if err != nil {
		return nil, mapInboundConflict(err)
	}
	return &source, nil
}

func (r *Postgres) GetInboundSource(ctx context.Context, id int64) (*webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	source, err := r.q.GetWebhookInboundSource(ctx, webhooksqlc.GetWebhookInboundSourceParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &source, nil
}

// GetInboundSourceByID resolves a source without a tenant context. It exists
// only for the public receiver, whose URL identifies the source and whose HMAC
// authenticates the caller. The returned row carries the owning tenant; every
// subsequent write must install that tenant in the context.
func (r *Postgres) GetInboundSourceByID(ctx context.Context, id int64) (*webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	source, err := r.q.GetWebhookInboundSourceByID(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &source, nil
}

func (r *Postgres) ListInboundSources(ctx context.Context, cur pagination.DBCursor) ([]webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	return r.q.ListWebhookInboundSources(ctx, webhooksqlc.ListWebhookInboundSourcesParams{
		TenantID:        tenantID(ctx),
		HasCursor:       cur.HasCursor,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.ID,
		RowLimit:        cur.Limit,
	})
}

func (r *Postgres) UpdateInboundSource(ctx context.Context, draft UpdateInboundSourceDraft) (*webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	source, err := r.q.UpdateWebhookInboundSource(ctx, webhooksqlc.UpdateWebhookInboundSourceParams{
		Name:     draft.Name,
		Slug:     draft.Slug,
		Status:   draft.Status,
		ID:       draft.ID,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, apperror.NewConflict("inbound source slug already exists")
		}
		return nil, mapNotFound(err)
	}
	return &source, nil
}

func (r *Postgres) DeleteInboundSource(ctx context.Context, id int64) (*webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	source, err := r.q.DeleteWebhookInboundSource(ctx, webhooksqlc.DeleteWebhookInboundSourceParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &source, nil
}

func (r *Postgres) RotateInboundSourceSecret(ctx context.Context, id int64, ciphertext string) (*webhooksqlc.WebhookInboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	source, err := r.q.RotateWebhookInboundSourceSecret(ctx, webhooksqlc.RotateWebhookInboundSourceSecretParams{
		SecretCiphertext: ciphertext,
		ID:               id,
		TenantID:         tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &source, nil
}

func (r *Postgres) CreateInboundEvent(ctx context.Context, draft InboundEventDraft) (eventID int64, created bool, err error) {
	if r == nil || r.q == nil {
		return 0, false, ErrNoPool
	}
	eventID, err = r.q.CreateWebhookInboundEvent(ctx, webhooksqlc.CreateWebhookInboundEventParams{
		TenantID:        tenantID(ctx),
		SourceID:        draft.SourceID,
		ExternalEventID: draft.ExternalEventID,
		EventType:       draft.EventType,
		Signature:       draft.Signature,
		Payload:         draft.Payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return eventID, true, nil
}

func (r *Postgres) GetInboundEventByExternalID(ctx context.Context, sourceID int64, externalEventID string) (*webhooksqlc.WebhookInboundEvent, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	event, err := r.q.GetWebhookInboundEventByExternalID(ctx, webhooksqlc.GetWebhookInboundEventByExternalIDParams{
		TenantID:        tenantID(ctx),
		SourceID:        sourceID,
		ExternalEventID: externalEventID,
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &event, nil
}

func (r *Postgres) ListInboundEvents(ctx context.Context, sourceID int64, cur pagination.DBCursor) ([]webhooksqlc.WebhookInboundEvent, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	return r.q.ListWebhookInboundEvents(ctx, webhooksqlc.ListWebhookInboundEventsParams{
		TenantID:         tenantID(ctx),
		SourceID:         sourceID,
		HasCursor:        cur.HasCursor,
		CursorReceivedAt: cur.CreatedAtParam(),
		CursorID:         cur.ID,
		RowLimit:         cur.Limit,
	})
}

func (r *Postgres) PurgeInboundEvents(ctx context.Context, retentionDays int32) (int64, error) {
	if r == nil || r.q == nil {
		return 0, ErrNoPool
	}
	return r.q.PurgeWebhookInboundEvents(ctx, retentionDays)
}
