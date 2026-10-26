package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	webhooksvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

func toDelivery(row webhooksqlc.WebhookDelivery) webhooksvc.Delivery {
	return webhooksvc.Delivery{
		ID:               row.ID,
		TenantID:         row.TenantID,
		EndpointID:       row.EndpointID,
		EventID:          row.EventID,
		URL:              row.Url,
		SecretCiphertext: row.SecretCiphertext,
		Status:           row.Status,
		Attempts:         row.Attempts,
		MaxAttempts:      row.MaxAttempts,
		TimeoutMS:        row.TimeoutMs,
		NextAttemptAt:    row.NextAttemptAt.Time,
		LastStatusCode:   intPtr(row.LastStatusCode),
		LastError:        row.LastError.String,
		DeliveredAt:      timePtr(row.DeliveredAt),
		ReplayedFromID:   int64Ptr(row.ReplayedFromID),
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
	}
}

func toDeliveryAttempt(row webhooksqlc.WebhookDeliveryAttempt) webhooksvc.DeliveryAttempt {
	return webhooksvc.DeliveryAttempt{
		ID:         row.ID,
		DeliveryID: row.DeliveryID,
		Attempt:    row.Attempt,
		StatusCode: intPtr(row.StatusCode),
		DurationMS: row.DurationMs,
		Error:      row.Error.String,
		CreatedAt:  row.CreatedAt.Time,
	}
}

// CreateDeliveryWithJob writes the event (first delivery of EventID only),
// the delivery row, and its first durable job in one transaction.
func (r *Postgres) CreateDeliveryWithJob(ctx context.Context, draft webhooksvc.DeliveryDraft) (deliveryID int64, created bool, err error) {
	if r == nil || r.db == nil {
		return 0, false, ErrNoPool
	}
	if r.queue == nil {
		return 0, false, ErrNoQueue
	}
	err = r.withTx(ctx, func(tx pgx.Tx, q *webhooksqlc.Queries) error {
		if err := q.CreateWebhookEvent(ctx, webhooksqlc.CreateWebhookEventParams{
			TenantID:    draft.TenantID,
			EventID:     draft.EventID,
			EventType:   draft.EventType,
			DataVersion: draft.DataVersion,
			ActorType:   draft.ActorType,
			ActorID:     text(draft.ActorID),
			OccurredAt:  timestamp(draft.EventCreatedAt),
			Payload:     draft.Payload,
		}); err != nil {
			return err
		}
		id, err := q.CreateWebhookDelivery(ctx, webhooksqlc.CreateWebhookDeliveryParams{
			TenantID:         draft.TenantID,
			EndpointID:       draft.EndpointID,
			EventID:          draft.EventID,
			Url:              draft.URL,
			SecretCiphertext: draft.SecretCiphertext,
			MaxAttempts:      draft.MaxAttempts,
			TimeoutMs:        draft.TimeoutMS,
			ReplayedFromID:   nullableInt8(draft.ReplayedFromID),
			IsReplay:         draft.IsReplay,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The event was already dispatched to this endpoint. Nothing was
			// inserted, so no job is enqueued.
			return nil
		}
		if err != nil {
			return err
		}
		if err := enqueueDeliveryJob(ctx, tx, r.queue, draft.TenantID, id, draft.RunAt); err != nil {
			return err
		}
		deliveryID = id
		created = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return deliveryID, created, nil
}

func (r *Postgres) GetDelivery(ctx context.Context, id int64) (*webhooksvc.Delivery, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	row, err := r.q.GetWebhookDelivery(ctx, webhooksqlc.GetWebhookDeliveryParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	delivery := toDelivery(row.WebhookDelivery)
	delivery.EventType = row.EventType
	delivery.DataVersion = row.DataVersion
	delivery.ActorType = row.ActorType
	delivery.ActorID = row.ActorID.String
	delivery.OccurredAt = row.OccurredAt.Time
	delivery.Payload = row.Payload
	return &delivery, nil
}

func (r *Postgres) ListDeliveries(ctx context.Context, params webhooksvc.ListDeliveriesParams) ([]webhooksvc.Delivery, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookDeliveries(ctx, webhooksqlc.ListWebhookDeliveriesParams{
		TenantID:        tenantID(ctx),
		EndpointID:      params.EndpointID,
		Status:          params.Status,
		CursorCreatedAt: params.Cursor.CreatedAtParam(),
		CursorID:        params.Cursor.IDParam(),
		RowLimit:        params.Cursor.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksvc.Delivery, len(rows))
	for i, row := range rows {
		delivery := toDelivery(row.WebhookDelivery)
		delivery.EventType = row.EventType
		delivery.OccurredAt = row.OccurredAt.Time
		out[i] = delivery
	}
	return out, nil
}

func (r *Postgres) ClaimDelivery(ctx context.Context, id int64, lease time.Duration) (*webhooksvc.Delivery, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	if lease < time.Second {
		lease = time.Minute
	}
	row, err := r.q.ClaimWebhookDelivery(ctx, webhooksqlc.ClaimWebhookDeliveryParams{
		LeaseSeconds: lease.Seconds(),
		ID:           id,
		TenantID:     tenantID(ctx),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, webhooksvc.ErrNotClaimable
	}
	if err != nil {
		return nil, err
	}
	return &webhooksvc.Delivery{
		ID:               row.ID,
		TenantID:         row.TenantID,
		EndpointID:       row.EndpointID,
		EventID:          row.EventID,
		URL:              row.Url,
		SecretCiphertext: row.SecretCiphertext,
		Status:           row.Status,
		Attempts:         row.Attempts,
		MaxAttempts:      row.MaxAttempts,
		TimeoutMS:        row.TimeoutMs,
		NextAttemptAt:    row.NextAttemptAt.Time,
		LastStatusCode:   intPtr(row.LastStatusCode),
		LastError:        row.LastError.String,
		DeliveredAt:      timePtr(row.DeliveredAt),
		ReplayedFromID:   int64Ptr(row.ReplayedFromID),
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
		EventType:        row.EventType,
		DataVersion:      row.DataVersion,
		ActorType:        row.ActorType,
		ActorID:          row.ActorID.String,
		OccurredAt:       row.OccurredAt.Time,
		Payload:          row.Payload,
	}, nil
}

// MarkDeliverySucceeded finishes the claimed attempt. It returns
// ErrDeliveryLeaseLost when the delivery is terminal or a newer attempt was
// claimed.
func (r *Postgres) MarkDeliverySucceeded(ctx context.Context, id int64, attempt int32, statusCode int) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	affected, err := r.q.MarkWebhookDeliverySucceeded(ctx, webhooksqlc.MarkWebhookDeliverySucceededParams{
		StatusCode: httpStatus(statusCode),
		ID:         id,
		TenantID:   tenantID(ctx),
		Attempts:   attempt,
	})
	return fencedResult(affected, err)
}

// MarkDeliveryDead dead-letters the claimed attempt, fenced like
// MarkDeliverySucceeded.
func (r *Postgres) MarkDeliveryDead(ctx context.Context, id int64, attempt int32, statusCode int, reason string) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	affected, err := r.q.MarkWebhookDeliveryDead(ctx, webhooksqlc.MarkWebhookDeliveryDeadParams{
		StatusCode: httpStatus(statusCode),
		LastError:  text(reason),
		ID:         id,
		TenantID:   tenantID(ctx),
		Attempts:   attempt,
	})
	return fencedResult(affected, err)
}

// ReleaseDeliveryClaim returns an interrupted claimed attempt to the budget
// and clears the claim lease, fenced like MarkDeliverySucceeded.
func (r *Postgres) ReleaseDeliveryClaim(ctx context.Context, id int64, attempt int32) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	affected, err := r.q.ReleaseWebhookDeliveryClaim(ctx, webhooksqlc.ReleaseWebhookDeliveryClaimParams{
		ID:       id,
		TenantID: tenantID(ctx),
		Attempts: attempt,
	})
	return fencedResult(affected, err)
}

func fencedResult(affected int64, err error) error {
	if err != nil {
		return err
	}
	if affected == 0 {
		return webhooksvc.ErrDeliveryLeaseLost
	}
	return nil
}

func (r *Postgres) RescheduleDeliveryWithJob(ctx context.Context, draft webhooksvc.RetryDraft) error {
	if r == nil || r.db == nil {
		return ErrNoPool
	}
	if r.queue == nil {
		return ErrNoQueue
	}
	tid := tenantID(ctx)
	return r.withTx(ctx, func(tx pgx.Tx, q *webhooksqlc.Queries) error {
		affected, err := q.RescheduleWebhookDelivery(ctx, webhooksqlc.RescheduleWebhookDeliveryParams{
			NextAttemptAt: timestamp(draft.NextAttemptAt),
			StatusCode:    httpStatus(draft.StatusCode),
			LastError:     text(draft.Error),
			ID:            draft.DeliveryID,
			TenantID:      tid,
			Attempts:      draft.Attempt,
		})
		if err != nil {
			return err
		}
		if affected == 0 {
			// The delivery became terminal, or a newer attempt was claimed,
			// between claim and reschedule. Do not enqueue work for it.
			return webhooksvc.ErrDeliveryLeaseLost
		}
		return enqueueDeliveryJob(ctx, tx, r.queue, tid, draft.DeliveryID, draft.NextAttemptAt)
	})
}

func (r *Postgres) CreateDeliveryAttempt(ctx context.Context, draft webhooksvc.AttemptDraft) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	_, err := r.q.CreateWebhookDeliveryAttempt(ctx, webhooksqlc.CreateWebhookDeliveryAttemptParams{
		TenantID:   tenantID(ctx),
		DeliveryID: draft.DeliveryID,
		Attempt:    draft.Attempt,
		StatusCode: httpStatus(draft.StatusCode),
		DurationMs: durationMillis(draft.Duration),
		Error:      text(draft.Error),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A duplicate attempt row is safe; the delivery is at-least-once.
		return nil
	}
	return err
}

func (r *Postgres) ListDeliveryAttempts(ctx context.Context, deliveryID int64) ([]webhooksvc.DeliveryAttempt, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	rows, err := r.q.ListWebhookDeliveryAttempts(ctx, webhooksqlc.ListWebhookDeliveryAttemptsParams{
		TenantID:   tenantID(ctx),
		DeliveryID: deliveryID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksvc.DeliveryAttempt, len(rows))
	for i, row := range rows {
		out[i] = toDeliveryAttempt(row)
	}
	return out, nil
}

func (r *Postgres) PurgeDeliveries(ctx context.Context, retentionDays int32) (int64, error) {
	if r == nil || r.q == nil {
		return 0, ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return purgeInBatches(ctx, func(ctx context.Context) (int64, error) {
		return r.q.PurgeWebhookDeliveries(ctx, webhooksqlc.PurgeWebhookDeliveriesParams{
			RetentionDays: retentionDays,
			BatchSize:     purgeBatchSize,
		})
	})
}

// PurgeOrphanEvents deletes events older than the retention window that no
// delivery references any more. Run it after PurgeDeliveries.
func (r *Postgres) PurgeOrphanEvents(ctx context.Context, retentionDays int32) (int64, error) {
	if r == nil || r.q == nil {
		return 0, ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return purgeInBatches(ctx, func(ctx context.Context) (int64, error) {
		return r.q.PurgeOrphanWebhookEvents(ctx, webhooksqlc.PurgeOrphanWebhookEventsParams{
			RetentionDays: retentionDays,
			BatchSize:     purgeBatchSize,
		})
	})
}

// purgeInBatches runs bounded retention batches until one is short (the
// backlog is drained) or ctx ends, so a large backlog never holds one long
// transaction or lock set. Rows deleted before an error are still returned.
func purgeInBatches(ctx context.Context, batch func(context.Context) (int64, error)) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("webhooks: purge: %w", err)
		}
		deleted, err := batch(ctx)
		if err != nil {
			return total, fmt.Errorf("webhooks: purge: %w", err)
		}
		total += deleted
		if deleted < purgeBatchSize {
			return total, nil
		}
	}
}

// ExpireUnclaimableDelivery dead-letters a pending delivery that can never be
// claimed again (inactive endpoint or exhausted attempts). It reports whether
// the delivery was expired; a live claim lease is never touched.
func (r *Postgres) ExpireUnclaimableDelivery(ctx context.Context, id int64) (bool, error) {
	if r == nil || r.q == nil {
		return false, ErrNoPool
	}
	affected, err := r.q.ExpireUnclaimableWebhookDelivery(ctx, webhooksqlc.ExpireUnclaimableWebhookDeliveryParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func httpStatus(code int) int32 {
	if code < 0 || code > 599 {
		return 0
	}
	return int32(code)
}

func durationMillis(d time.Duration) int32 {
	ms := d.Milliseconds()
	if ms < 0 {
		return 0
	}
	if ms > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(ms)
}

func text(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}
