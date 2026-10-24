package repository

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
)

func (r *Postgres) CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID int64, created bool, err error) {
	if r == nil || r.pool == nil {
		return 0, false, ErrNoPool
	}
	if r.queue == nil {
		return 0, false, ErrNoQueue
	}
	err = r.withTx(ctx, func(tx pgx.Tx, q *webhooksqlc.Queries) error {
		id, err := q.CreateWebhookDelivery(ctx, webhooksqlc.CreateWebhookDeliveryParams{
			TenantID:         draft.TenantID,
			EndpointID:       draft.EndpointID,
			EventID:          draft.EventID,
			EventType:        draft.EventType,
			EventCreatedAt:   timestamp(draft.EventCreatedAt),
			Url:              draft.URL,
			Payload:          draft.Payload,
			SecretCiphertext: draft.SecretCiphertext,
			MaxAttempts:      draft.MaxAttempts,
			ReplayedFromID:   nullableInt8(draft.ReplayedFromID),
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

func (r *Postgres) GetDelivery(ctx context.Context, id int64) (*webhooksqlc.WebhookDelivery, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	delivery, err := r.q.GetWebhookDelivery(ctx, webhooksqlc.GetWebhookDeliveryParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &delivery, nil
}

func (r *Postgres) ListDeliveries(ctx context.Context, params ListDeliveriesParams) ([]webhooksqlc.WebhookDelivery, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	return r.q.ListWebhookDeliveries(ctx, webhooksqlc.ListWebhookDeliveriesParams{
		TenantID:        tenantID(ctx),
		EndpointID:      params.EndpointID,
		Status:          params.Status,
		HasCursor:       params.Cursor.HasCursor,
		CursorCreatedAt: params.Cursor.CreatedAtParam(),
		CursorID:        params.Cursor.ID,
		RowLimit:        params.Cursor.Limit,
	})
}

func (r *Postgres) ClaimDelivery(ctx context.Context, id int64, lease time.Duration) (*webhooksqlc.WebhookDelivery, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	if lease < time.Second {
		lease = time.Minute
	}
	delivery, err := r.q.ClaimWebhookDelivery(ctx, webhooksqlc.ClaimWebhookDeliveryParams{
		LeaseSeconds: lease.Seconds(),
		ID:           id,
		TenantID:     tenantID(ctx),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotClaimable
	}
	if err != nil {
		return nil, err
	}
	return &delivery, nil
}

func (r *Postgres) MarkDeliverySucceeded(ctx context.Context, id int64, statusCode int) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	return r.q.MarkWebhookDeliverySucceeded(ctx, webhooksqlc.MarkWebhookDeliverySucceededParams{
		StatusCode: httpStatus(statusCode),
		ID:         id,
		TenantID:   tenantID(ctx),
	})
}

func (r *Postgres) MarkDeliveryDead(ctx context.Context, id int64, statusCode int, reason string) error {
	if r == nil || r.q == nil {
		return ErrNoPool
	}
	return r.q.MarkWebhookDeliveryDead(ctx, webhooksqlc.MarkWebhookDeliveryDeadParams{
		StatusCode: httpStatus(statusCode),
		LastError:  text(reason),
		ID:         id,
		TenantID:   tenantID(ctx),
	})
}

func (r *Postgres) RescheduleDeliveryWithJob(ctx context.Context, draft RetryDraft) error {
	if r == nil || r.pool == nil {
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
		})
		if err != nil {
			return err
		}
		if affected == 0 {
			// The delivery became terminal between claim and reschedule. Do not
			// enqueue work for a finished delivery.
			return nil
		}
		return enqueueDeliveryJob(ctx, tx, r.queue, tid, draft.DeliveryID, draft.NextAttemptAt)
	})
}

func (r *Postgres) CreateDeliveryAttempt(ctx context.Context, draft AttemptDraft) error {
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

func (r *Postgres) ListDeliveryAttempts(ctx context.Context, deliveryID int64) ([]webhooksqlc.WebhookDeliveryAttempt, error) {
	if r == nil || r.q == nil {
		return nil, ErrNoPool
	}
	return r.q.ListWebhookDeliveryAttempts(ctx, webhooksqlc.ListWebhookDeliveryAttemptsParams{
		TenantID:   tenantID(ctx),
		DeliveryID: deliveryID,
	})
}

func (r *Postgres) PurgeDeliveries(ctx context.Context, retentionDays int32) (int64, error) {
	if r == nil || r.q == nil {
		return 0, ErrNoPool
	}
	return r.q.PurgeWebhookDeliveries(ctx, retentionDays)
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
