package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	webhooksvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func toInboundSource(row webhooksqlc.WebhookInboundSource) webhooksvc.InboundSource {
	return webhooksvc.InboundSource{
		ID:        row.ID,
		TenantID:  row.TenantID,
		Name:      row.Name,
		Slug:      row.Slug,
		Status:    row.Status,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
		DeletedAt: database.TimePtr(row.DeletedAt),
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

// CreateInboundSource inserts the source and its version-1 secret in one
// transaction. The secret's AAD binds the source id the database generated.
func (r *Postgres) CreateInboundSource(ctx context.Context, draft webhooksvc.InboundSourceDraft) (*webhooksvc.InboundSource, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, ErrNoPool
	}
	if draft.SealSecret == nil {
		return nil, fmt.Errorf("webhooks: create inbound source: secret sealer is required")
	}
	status := draft.Status
	if status == "" {
		status = webhooksvc.StatusActive
	}
	tid := tenantID
	var source *webhooksvc.InboundSource
	err = r.withTx(ctx, func(_ pgx.Tx, q *webhooksqlc.Queries) error {
		created, err := q.CreateWebhookInboundSource(ctx, webhooksqlc.CreateWebhookInboundSourceParams{
			TenantID: tid,
			Name:     draft.Name,
			Slug:     draft.Slug,
			Status:   status,
		})
		if err != nil {
			return mapInboundConflict(err)
		}
		ciphertext, err := draft.SealSecret(created.ID)
		if err != nil {
			return err
		}
		if _, err := q.CreateWebhookInboundSourceSecret(ctx, webhooksqlc.CreateWebhookInboundSourceSecretParams{
			TenantID:         tid,
			SourceID:         created.ID,
			Version:          1,
			SecretCiphertext: ciphertext,
		}); err != nil {
			return err
		}
		mapped := toInboundSource(created)
		source = &mapped
		return nil
	})
	if err != nil {
		return nil, err
	}
	return source, nil
}

func (r *Postgres) GetInboundSource(ctx context.Context, id uuid.UUID) (*webhooksvc.InboundSource, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.GetWebhookInboundSource(ctx, webhooksqlc.GetWebhookInboundSourceParams{
		ID:       id,
		TenantID: tenantID,
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
func (r *Postgres) GetInboundSourceByID(ctx context.Context, id uuid.UUID) (*webhooksvc.InboundSource, error) {
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
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookInboundSources(ctx, webhooksqlc.ListWebhookInboundSourcesParams{
		TenantID:        tenantID,
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
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	var row webhooksqlc.WebhookInboundSource
	if draft.ExpectedUpdatedAt != nil {
		row, err = r.q.UpdateWebhookInboundSourceIfUnchanged(ctx, webhooksqlc.UpdateWebhookInboundSourceIfUnchangedParams{
			Name:              draft.Name,
			Slug:              draft.Slug,
			Status:            draft.Status,
			ID:                draft.ID,
			TenantID:          tenantID,
			ExpectedUpdatedAt: timestamptzValue(*draft.ExpectedUpdatedAt),
		})
	} else {
		row, err = r.q.UpdateWebhookInboundSource(ctx, webhooksqlc.UpdateWebhookInboundSourceParams{
			Name:     draft.Name,
			Slug:     draft.Slug,
			Status:   draft.Status,
			ID:       draft.ID,
			TenantID: tenantID,
		})
	}
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, apperror.NewConflict("inbound source slug already exists")
		}
		return nil, mapConditionalUpdate(ctx, r.q, err, func() error {
			_, existsErr := r.q.GetWebhookInboundSource(ctx, webhooksqlc.GetWebhookInboundSourceParams{ID: draft.ID, TenantID: tenantID})
			return existsErr
		})
	}
	source := toInboundSource(row)
	return &source, nil
}

func (r *Postgres) DeleteInboundSource(ctx context.Context, id uuid.UUID) (*webhooksvc.InboundSource, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.DeleteWebhookInboundSource(ctx, webhooksqlc.DeleteWebhookInboundSourceParams{
		ID:       id,
		TenantID: tenantID,
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	source := toInboundSource(row)
	return &source, nil
}

// RotateInboundSourceSecret retires the active secret with an explicit grace
// window and inserts the next version. The source row lock serializes
// concurrent rotations so they cannot claim the same version. expirePrevious
// closes the grace window immediately, so only the new secret verifies
// afterwards.
func (r *Postgres) RotateInboundSourceSecret(ctx context.Context, id uuid.UUID, ciphertext string, expirePrevious bool) (*webhooksvc.InboundSource, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, ErrNoPool
	}
	tid := tenantID
	var source *webhooksvc.InboundSource
	err = r.withTx(ctx, func(_ pgx.Tx, q *webhooksqlc.Queries) error {
		current, err := q.LockWebhookInboundSource(ctx, webhooksqlc.LockWebhookInboundSourceParams{
			ID:       id,
			TenantID: tid,
		})
		if err != nil {
			return mapNotFound(err)
		}
		version, err := q.NextWebhookInboundSourceSecretVersion(ctx, id)
		if err != nil {
			return err
		}
		if err := q.ExpireOlderWebhookInboundSourceSecrets(ctx, webhooksqlc.ExpireOlderWebhookInboundSourceSecretsParams{
			TenantID: tid,
			SourceID: id,
		}); err != nil {
			return err
		}
		graceSeconds := webhooksvc.SecretRotationGrace.Seconds()
		if expirePrevious {
			// A zero-length grace retires the replaced secret at commit time,
			// so the sender must switch to the new secret immediately.
			graceSeconds = 0
		}
		if err := q.DeactivateWebhookInboundSourceSecrets(ctx, webhooksqlc.DeactivateWebhookInboundSourceSecretsParams{
			TenantID:     tid,
			SourceID:     id,
			GraceSeconds: graceSeconds,
		}); err != nil {
			return err
		}
		if _, err := q.CreateWebhookInboundSourceSecret(ctx, webhooksqlc.CreateWebhookInboundSourceSecretParams{
			TenantID:         tid,
			SourceID:         id,
			Version:          version,
			SecretCiphertext: ciphertext,
		}); err != nil {
			return err
		}
		mapped := toInboundSource(current)
		source = &mapped
		return nil
	})
	if err != nil {
		return nil, err
	}
	return source, nil
}

// InboundSigningSecrets returns the source's active and unexpired previous
// sealed secrets. The caller must have installed the source tenant in ctx.
func (r *Postgres) InboundSigningSecrets(ctx context.Context, sourceID uuid.UUID) (*webhooksvc.SigningSecrets, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	secrets, err := r.q.GetWebhookInboundSourceSigningSecrets(ctx, webhooksqlc.GetWebhookInboundSourceSigningSecretsParams{
		TenantID: tenantID,
		SourceID: sourceID,
	})
	if err != nil {
		return nil, err
	}
	if secrets.CurrentCiphertext == "" {
		return nil, apperror.ErrNotFound
	}
	return &webhooksvc.SigningSecrets{
		CurrentCiphertext:  secrets.CurrentCiphertext,
		PreviousCiphertext: secrets.PreviousCiphertext,
	}, nil
}

func (r *Postgres) CreateInboundEvent(ctx context.Context, draft webhooksvc.InboundEventDraft) (eventID uuid.UUID, created bool, err error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	if r == nil || r.q == nil {
		return uuid.Nil, false, ErrNoPool
	}
	eventID, err = r.q.CreateWebhookInboundEvent(ctx, webhooksqlc.CreateWebhookInboundEventParams{
		TenantID:        tenantID,
		SourceID:        draft.SourceID,
		ExternalEventID: draft.ExternalEventID,
		EventType:       draft.EventType,
		Signature:       draft.Signature,
		Payload:         draft.Payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return eventID, true, nil
}

func (r *Postgres) GetInboundEventByExternalID(ctx context.Context, sourceID uuid.UUID, externalEventID string) (*webhooksvc.InboundEvent, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.GetWebhookInboundEventByExternalID(ctx, webhooksqlc.GetWebhookInboundEventByExternalIDParams{
		TenantID:        tenantID,
		SourceID:        sourceID,
		ExternalEventID: externalEventID,
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	event := toInboundEvent(row)
	return &event, nil
}

func (r *Postgres) ListInboundEvents(ctx context.Context, sourceID uuid.UUID, cur pagination.DBCursor) ([]webhooksvc.InboundEvent, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookInboundEvents(ctx, webhooksqlc.ListWebhookInboundEventsParams{
		TenantID:         tenantID,
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

// CreateInboundEventAudited stores a new inbound event and its required Tier 1
// audit row in one transaction. The action literal lives here beside
// RecordInTx so test/arch can prove the audit is transactional; the tenant and
// resource come from the authenticated source, because a public receiver has
// no tenant middleware.
func (r *Postgres) CreateInboundEventAudited(
	ctx context.Context,
	draft webhooksvc.InboundEventDraft,
	tenantID uuid.UUID,
	metadata map[string]any,
) (eventID uuid.UUID, created bool, err error) {
	err = r.withTx(ctx, func(tx pgx.Tx, q *webhooksqlc.Queries) error {
		eventID, created, err = createInboundEvent(ctx, q, draft)
		if err != nil || !created {
			return err
		}
		return audit.RecordInTx(ctx, r.audit, tx, audit.Event{
			TenantID:   tenantID,
			ActorType:  audit.ActorTypeService,
			Action:     "webhook.received",
			Resource:   "webhook_inbound_event",
			ResourceID: eventID.String(),
			Metadata:   metadata,
		})
	})
	return eventID, created, err
}

func createInboundEvent(ctx context.Context, q *webhooksqlc.Queries, draft webhooksvc.InboundEventDraft) (uuid.UUID, bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	eventID, err := q.CreateWebhookInboundEvent(ctx, webhooksqlc.CreateWebhookInboundEventParams{
		TenantID:        tenantID,
		SourceID:        draft.SourceID,
		ExternalEventID: draft.ExternalEventID,
		EventType:       draft.EventType,
		Signature:       draft.Signature,
		Payload:         draft.Payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return eventID, true, nil
}
