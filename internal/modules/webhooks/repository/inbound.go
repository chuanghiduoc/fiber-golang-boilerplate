package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	webhooksvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func toInboundSource(row webhooksqlc.WebhookInboundSource) webhooksvc.InboundSource {
	return webhooksvc.InboundSource{
		ID:               row.ID,
		TenantID:         row.TenantID,
		Name:             row.Name,
		Slug:             row.Slug,
		SecretCiphertext: row.SecretCiphertext,
		Status:           row.Status,
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
		DeletedAt:        timePtr(row.DeletedAt),
	}
}

func toInboundEvent(row webhooksqlc.WebhookInboundEvent) webhooksvc.InboundEvent {
	return webhooksvc.InboundEvent{
		ID:              row.ID,
		TenantID:        row.TenantID,
		SourceID:        row.SourceID,
		ExternalEventID: row.ExternalEventID,
		EventType:       row.EventType,
		Payload:         row.Payload,
		ReceivedAt:      row.ReceivedAt.Time,
	}
}

func (r *Postgres) CreateInboundSource(ctx context.Context, draft webhooksvc.InboundSourceDraft) (*webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.CreateWebhookInboundSource(ctx, webhooksqlc.CreateWebhookInboundSourceParams{
		TenantID:         tenantID(ctx),
		Name:             draft.Name,
		Slug:             draft.Slug,
		SecretCiphertext: draft.SecretCiphertext,
	})
	if err != nil {
		return nil, mapInboundConflict(err)
	}
	source := toInboundSource(row)
	return &source, nil
}

func (r *Postgres) GetInboundSource(ctx context.Context, id int64) (*webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.GetWebhookInboundSource(ctx, webhooksqlc.GetWebhookInboundSourceParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	source := toInboundSource(row)
	return &source, nil
}

// GetInboundSourceByID resolves a source without a tenant context. It exists
// only for the public receiver, whose URL identifies the source and whose HMAC
// authenticates the caller. The returned row carries the owning tenant; every
// subsequent write must install that tenant in the context.
func (r *Postgres) GetInboundSourceByID(ctx context.Context, id int64) (*webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // The public receiver resolves a source by id before it knows the tenant.
	row, err := r.q.GetWebhookInboundSourceByID(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	source := toInboundSource(row)
	return &source, nil
}

func (r *Postgres) ListInboundSources(ctx context.Context, cur pagination.DBCursor) ([]webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookInboundSources(ctx, webhooksqlc.ListWebhookInboundSourcesParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksvc.InboundSource, len(rows))
	for i, row := range rows {
		out[i] = toInboundSource(row)
	}
	return out, nil
}

func (r *Postgres) UpdateInboundSource(ctx context.Context, draft webhooksvc.UpdateInboundSourceDraft) (*webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.UpdateWebhookInboundSource(ctx, webhooksqlc.UpdateWebhookInboundSourceParams{
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
	source := toInboundSource(row)
	return &source, nil
}

func (r *Postgres) DeleteInboundSource(ctx context.Context, id int64) (*webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.DeleteWebhookInboundSource(ctx, webhooksqlc.DeleteWebhookInboundSourceParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	source := toInboundSource(row)
	return &source, nil
}

func (r *Postgres) RotateInboundSourceSecret(ctx context.Context, id int64, ciphertext string) (*webhooksvc.InboundSource, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.RotateWebhookInboundSourceSecret(ctx, webhooksqlc.RotateWebhookInboundSourceSecretParams{
		SecretCiphertext: ciphertext,
		ID:               id,
		TenantID:         tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	source := toInboundSource(row)
	return &source, nil
}

func (r *Postgres) CreateInboundEvent(ctx context.Context, draft webhooksvc.InboundEventDraft) (eventID int64, created bool, err error) {
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

func (r *Postgres) GetInboundEventByExternalID(ctx context.Context, sourceID int64, externalEventID string) (*webhooksvc.InboundEvent, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.GetWebhookInboundEventByExternalID(ctx, webhooksqlc.GetWebhookInboundEventByExternalIDParams{
		TenantID:        tenantID(ctx),
		SourceID:        sourceID,
		ExternalEventID: externalEventID,
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	event := toInboundEvent(row)
	return &event, nil
}

func (r *Postgres) ListInboundEvents(ctx context.Context, sourceID int64, cur pagination.DBCursor) ([]webhooksvc.InboundEvent, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookInboundEvents(ctx, webhooksqlc.ListWebhookInboundEventsParams{
		TenantID:         tenantID(ctx),
		SourceID:         sourceID,
		CursorReceivedAt: cur.CreatedAtParam(),
		CursorID:         cur.IDParam(),
		RowLimit:         cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksvc.InboundEvent, len(rows))
	for i, row := range rows {
		out[i] = toInboundEvent(row)
	}
	return out, nil
}

func (r *Postgres) PurgeInboundEvents(ctx context.Context, retentionDays int32) (int64, error) {
	if r == nil || r.q == nil {
		return 0, ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return purgeInBatches(ctx, func(ctx context.Context) (int64, error) {
		return r.q.PurgeWebhookInboundEvents(ctx, webhooksqlc.PurgeWebhookInboundEventsParams{
			RetentionDays: retentionDays,
			BatchSize:     purgeBatchSize,
		})
	})
}
