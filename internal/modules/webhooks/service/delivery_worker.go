package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const (
	// DeliveryJobKind is the durable job kind processed by DeliveryWorker.
	DeliveryJobKind = repository.DeliveryJobKind
	defaultLease    = 3 * time.Minute
	maxErrorLen     = 1000
	// bookkeepingTimeout bounds the detached writes that record an attempt
	// after the HTTP request was sent.
	bookkeepingTimeout = 5 * time.Second
)

// WorkerOptions configures retry and claim behavior.
type WorkerOptions struct {
	RetryBase time.Duration
	RetryMax  time.Duration
	Lease     time.Duration
}

// DeliveryEnvelope is the stable JSON body signed and delivered to receivers.
type DeliveryEnvelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"createdAt"`
	Data      json.RawMessage `json:"data"`
}

// WorkerStore is the narrow persistence surface for delivery attempts.
type WorkerStore interface {
	ClaimDelivery(ctx context.Context, id int64, lease time.Duration) (*webhooksqlc.WebhookDelivery, error)
	GetDelivery(ctx context.Context, id int64) (*webhooksqlc.WebhookDelivery, error)
	MarkDeliverySucceeded(ctx context.Context, id int64, statusCode int) error
	MarkDeliveryDead(ctx context.Context, id int64, statusCode int, reason string) error
	ExpireUnclaimableDelivery(ctx context.Context, id int64) (bool, error)
	RescheduleDeliveryWithJob(ctx context.Context, draft repository.RetryDraft) error
	CreateDeliveryAttempt(ctx context.Context, draft repository.AttemptDraft) error
}

// DeliveryWorker handles one webhook.delivery job: claim an attempt lease,
// sign and send exactly one HTTP request, record the attempt, and either finish,
// dead-letter, or schedule the next attempt with Retry-After/backoff.
type DeliveryWorker struct {
	store  WorkerStore
	codec  *SecretCodec
	client *webhook.Client
	opts   WorkerOptions
	now    func() time.Time
}

// NewDeliveryWorker returns a worker. Nil client becomes a default SSRF-safe
// client with no allowlist, which blocks loopback/private receivers.
func NewDeliveryWorker(store WorkerStore, codec *SecretCodec, client *webhook.Client, opts WorkerOptions) *DeliveryWorker {
	if opts.RetryBase <= 0 {
		opts.RetryBase = 5 * time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = time.Hour
	}
	if opts.RetryMax < opts.RetryBase {
		opts.RetryMax = opts.RetryBase
	}
	if opts.Lease <= 0 {
		opts.Lease = defaultLease
	}
	if client == nil {
		client = webhook.NewClient(webhook.ClientConfig{})
	}
	return &DeliveryWorker{
		store:  store,
		codec:  codec,
		client: client,
		opts:   opts,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Handle is registered on the platform jobs worker under DeliveryJobKind.
func (w *DeliveryWorker) Handle(ctx context.Context, payload json.RawMessage) error {
	if w == nil || w.store == nil {
		return errors.New("webhooks: delivery worker is not configured")
	}
	if w.codec == nil {
		return errors.New("webhooks: delivery worker has no secret codec")
	}
	var job repository.DeliveryJobPayload
	if err := json.Unmarshal(payload, &job); err != nil || job.DeliveryID <= 0 {
		return fmt.Errorf("webhooks: invalid delivery job payload: %w", err)
	}

	delivery, err := w.store.ClaimDelivery(ctx, job.DeliveryID, w.opts.Lease)
	if err != nil {
		if errors.Is(err, repository.ErrNotClaimable) {
			return w.handleNotClaimable(ctx, job.DeliveryID)
		}
		return fmt.Errorf("webhooks: claim delivery %d: %w", job.DeliveryID, err)
	}

	secret, err := w.codec.Open(delivery.SecretCiphertext)
	if err != nil {
		reason := "secret snapshot could not be decrypted"
		slog.Error("webhooks: delivery secret decryption failed",
			slog.Int64("delivery_id", delivery.ID),
			slog.Any("error", err),
		)
		_ = w.store.CreateDeliveryAttempt(ctx, repository.AttemptDraft{
			DeliveryID: delivery.ID,
			Attempt:    delivery.Attempts,
			Error:      reason,
		})
		if markErr := w.store.MarkDeliveryDead(ctx, delivery.ID, 0, reason); markErr != nil {
			return markErr
		}
		return nil
	}

	body, err := json.Marshal(DeliveryEnvelope{
		ID:        delivery.EventID,
		Type:      delivery.EventType,
		CreatedAt: delivery.EventCreatedAt.Time.UTC(),
		Data:      json.RawMessage(delivery.Payload),
	})
	if err != nil {
		reason := "delivery payload is not valid JSON"
		if attemptErr := w.store.CreateDeliveryAttempt(ctx, repository.AttemptDraft{
			DeliveryID: delivery.ID,
			Attempt:    delivery.Attempts,
			Error:      reason,
		}); attemptErr != nil {
			return attemptErr
		}
		return w.store.MarkDeliveryDead(ctx, delivery.ID, 0, reason)
	}

	timestamp := w.now()
	reqHeaders := http.Header{}
	reqHeaders.Set(webhook.HeaderID, delivery.EventID)
	reqHeaders.Set(webhook.HeaderDelivery, strconv.FormatInt(delivery.ID, 10))
	reqHeaders.Set(webhook.HeaderEvent, delivery.EventType)
	reqHeaders.Set(webhook.HeaderTimestamp, strconv.FormatInt(timestamp.Unix(), 10))
	reqHeaders.Set(webhook.HeaderSignature, webhook.SignatureHeader(
		webhook.Sign(secret, delivery.EventID, delivery.EventType, timestamp, body)))
	reqHeaders.Set(webhook.HeaderAttempt, strconv.FormatInt(int64(delivery.Attempts), 10))
	reqHeaders.Set("Content-Type", "application/json")

	// The endpoint timeout (snapshotted on the delivery) can only shorten the
	// client's global request timeout.
	reqCtx, cancelReq := context.WithTimeout(ctx, deliveryTimeout(delivery.TimeoutMs))
	started := time.Now()
	result, doErr := w.client.Do(reqCtx, delivery.Url, reqHeaders, body)
	elapsed := time.Since(started)
	cancelReq()

	// The request has been sent, so its outcome must be recorded even if the
	// job is being canceled for shutdown; otherwise the attempt is lost and a
	// retry can deliver a duplicate before the lease even expires.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()

	if doErr != nil {
		outcome := webhook.ClassifyError(doErr)
		statusCode := 0
		reason := truncateError(fmt.Sprintf("request failed: %v", doErr))
		if err := w.store.CreateDeliveryAttempt(ctx, repository.AttemptDraft{
			DeliveryID: delivery.ID,
			Attempt:    delivery.Attempts,
			StatusCode: statusCode,
			Duration:   elapsed,
			Error:      reason,
		}); err != nil {
			return fmt.Errorf("webhooks: record delivery attempt: %w", err)
		}
		return w.finish(ctx, delivery, outcome, statusCode, reason, "")
	}

	outcome := webhook.ClassifyStatus(result.StatusCode)
	reason := ""
	if outcome != webhook.OutcomeSuccess {
		reason = webhook.StatusError(result.StatusCode)
	}
	if err := w.store.CreateDeliveryAttempt(ctx, repository.AttemptDraft{
		DeliveryID: delivery.ID,
		Attempt:    delivery.Attempts,
		StatusCode: result.StatusCode,
		Duration:   elapsed,
		Error:      reason,
	}); err != nil {
		return fmt.Errorf("webhooks: record delivery attempt: %w", err)
	}
	return w.finish(ctx, delivery, outcome, result.StatusCode, reason, result.Header.Get("Retry-After"))
}

func (w *DeliveryWorker) finish(ctx context.Context, delivery *webhooksqlc.WebhookDelivery, outcome webhook.Outcome, statusCode int, reason, retryAfter string) error {
	switch outcome {
	case webhook.OutcomeSuccess:
		if err := w.store.MarkDeliverySucceeded(ctx, delivery.ID, statusCode); err != nil {
			return fmt.Errorf("webhooks: mark delivery succeeded: %w", err)
		}
		return nil

	case webhook.OutcomePermanent:
		if err := w.store.MarkDeliveryDead(ctx, delivery.ID, statusCode, reason); err != nil {
			return fmt.Errorf("webhooks: mark delivery dead: %w", err)
		}
		return nil

	default:
		if delivery.Attempts >= delivery.MaxAttempts {
			if err := w.store.MarkDeliveryDead(ctx, delivery.ID, statusCode, reason); err != nil {
				return fmt.Errorf("webhooks: mark delivery dead after retries: %w", err)
			}
			return nil
		}
		now := w.now()
		delay, _ := webhook.RetryDelay(int(delivery.Attempts), w.opts.RetryBase, w.opts.RetryMax, retryAfter, now)
		if delay < 0 {
			delay = 0
		}
		next := now.Add(delay)
		if err := w.store.RescheduleDeliveryWithJob(ctx, repository.RetryDraft{
			DeliveryID:    delivery.ID,
			NextAttemptAt: next,
			StatusCode:    statusCode,
			Error:         reason,
		}); err != nil {
			return fmt.Errorf("webhooks: reschedule delivery: %w", err)
		}
		return nil
	}
}

// handleNotClaimable distinguishes an already-terminal delivery (nothing to
// do) from a pending delivery whose claim lease is still held. The latter
// returns an error so the platform job retries after the lease expires; that
// is the crash-recovery path after a process dies mid-attempt.
func (w *DeliveryWorker) handleNotClaimable(ctx context.Context, id int64) error {
	delivery, err := w.store.GetDelivery(ctx, id)
	if err != nil {
		// A missing row may have been purged by retention; the job is done.
		if errors.Is(err, apperror.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("webhooks: load delivery %d: %w", id, err)
	}
	if delivery.Status != "pending" {
		return nil
	}
	// A pending delivery that can never be claimed again (endpoint disabled or
	// deleted, or every attempt consumed but the final bookkeeping lost) would
	// otherwise stay pending forever once this job dead-letters.
	expired, err := w.store.ExpireUnclaimableDelivery(ctx, id)
	if err != nil {
		return fmt.Errorf("webhooks: expire delivery %d: %w", id, err)
	}
	if expired {
		slog.Warn("webhooks: dead-lettered unclaimable delivery", slog.Int64("delivery_id", id))
		return nil
	}
	return fmt.Errorf("webhooks: delivery %d claim is busy", id)
}

// deliveryTimeout converts a snapshotted endpoint timeout, falling back to the
// schema default for rows that predate the snapshot.
func deliveryTimeout(timeoutMS int32) time.Duration {
	if timeoutMS <= 0 {
		return defaultTimeoutMS * time.Millisecond
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func truncateError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxErrorLen {
		return value
	}
	return value[:maxErrorLen]
}
