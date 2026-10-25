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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
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
	// secretRotationGrace is how long after an endpoint secret rotation a
	// delivery whose snapshot predates the rotation is signed with both the
	// snapshot and the current secret. Receivers can switch to the new secret
	// at any time inside the window without failing pending retries; after it
	// only the current secret signs.
	secretRotationGrace = 24 * time.Hour
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
	ClaimDelivery(ctx context.Context, id int64, lease time.Duration) (*repository.Delivery, error)
	GetDelivery(ctx context.Context, id int64) (*repository.Delivery, error)
	MarkDeliverySucceeded(ctx context.Context, id int64, attempt int32, statusCode int) error
	MarkDeliveryDead(ctx context.Context, id int64, attempt int32, statusCode int, reason string) error
	ReleaseDeliveryClaim(ctx context.Context, id int64, attempt int32) error
	ActiveSigningSecret(ctx context.Context, endpointID int64) (*repository.SigningSecret, error)
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

// Handle is registered on the platform jobs worker under DeliveryJobKind. It
// follows the delivery protocol step by step: claim the attempt, build and
// sign the body, send one request, then classify and record the outcome.
func (w *DeliveryWorker) Handle(ctx context.Context, payload json.RawMessage) error {
	if err := w.checkConfigured(); err != nil {
		return err
	}
	// An undecodable payload or a missing delivery id can never succeed.
	var job repository.DeliveryJobPayload
	if err := json.Unmarshal(payload, &job); err != nil {
		return jobs.Permanent(fmt.Errorf("webhooks: invalid delivery job payload: %w", err))
	}
	if job.DeliveryID <= 0 {
		return jobs.Permanent(errors.New("webhooks: invalid delivery job payload: missing delivery id"))
	}

	delivery, err := w.store.ClaimDelivery(ctx, job.DeliveryID, w.opts.Lease)
	if errors.Is(err, repository.ErrNotClaimable) {
		return w.handleNotClaimable(ctx, job.DeliveryID)
	}
	if err != nil {
		return fmt.Errorf("webhooks: claim delivery %d: %w", job.DeliveryID, err)
	}

	secret, body, unsendable := w.prepare(delivery)
	if unsendable != "" {
		return w.deadLetterUnsendable(ctx, delivery, unsendable)
	}

	headers := w.requestHeaders(ctx, delivery, secret, body)
	sent := w.send(ctx, delivery, headers, body)
	// The job context itself ended (worker shutdown or job timeout), as
	// opposed to the per-endpoint request timeout, before any response
	// arrived. That says nothing about the receiver.
	interrupted := sent.err != nil && ctx.Err() != nil
	jobErr := ctx.Err()

	// The request has been sent, so its outcome must be recorded even if the
	// job is being canceled for shutdown; otherwise the attempt is lost and a
	// retry can deliver a duplicate before the lease even expires.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()

	if interrupted {
		return w.releaseInterrupted(writeCtx, delivery, jobErr)
	}
	attempt := classifyAttempt(sent)
	if err := w.recordAttempt(writeCtx, delivery, attempt); err != nil {
		return err
	}
	return w.finish(writeCtx, delivery, attempt)
}

func (w *DeliveryWorker) checkConfigured() error {
	if w == nil || w.store == nil {
		return errors.New("webhooks: delivery worker is not configured")
	}
	if w.codec == nil {
		return errors.New("webhooks: delivery worker has no secret codec")
	}
	return nil
}

// prepare opens the snapshotted secret and builds the signed envelope. A
// non-empty unsendable reason means retrying cannot help: the delivery is
// dead-lettered without sending anything.
func (w *DeliveryWorker) prepare(delivery *repository.Delivery) (secret, body []byte, unsendable string) {
	secret, err := w.codec.Open(delivery.SecretCiphertext)
	if err != nil {
		slog.Error("webhooks: delivery secret decryption failed",
			slog.Int64("delivery_id", delivery.ID),
			slog.Any("error", err),
		)
		return nil, nil, "secret snapshot could not be decrypted"
	}
	body, err = json.Marshal(DeliveryEnvelope{
		ID:        delivery.EventID,
		Type:      delivery.EventType,
		CreatedAt: delivery.OccurredAt.Time.UTC(),
		Data:      json.RawMessage(delivery.Payload),
	})
	if err != nil {
		return nil, nil, "delivery payload is not valid JSON"
	}
	return secret, body, ""
}

// deadLetterUnsendable records the failed attempt and dead-letters a
// delivery that could not even be built.
func (w *DeliveryWorker) deadLetterUnsendable(ctx context.Context, delivery *repository.Delivery, reason string) error {
	if err := w.recordAttempt(ctx, delivery, attemptResult{outcome: webhook.OutcomePermanent, reason: reason}); err != nil {
		return err
	}
	return w.fenced(delivery, "mark delivery dead",
		w.store.MarkDeliveryDead(ctx, delivery.ID, delivery.Attempts, 0, reason))
}

// requestHeaders builds the delivery headers, including the timestamped
// signature over the exact body that will be sent.
func (w *DeliveryWorker) requestHeaders(ctx context.Context, delivery *repository.Delivery, secret, body []byte) http.Header {
	timestamp := w.now()
	headers := http.Header{}
	headers.Set(webhook.HeaderID, delivery.EventID)
	headers.Set(webhook.HeaderDelivery, strconv.FormatInt(delivery.ID, 10))
	headers.Set(webhook.HeaderEvent, delivery.EventType)
	headers.Set(webhook.HeaderTimestamp, strconv.FormatInt(timestamp.Unix(), 10))
	headers.Set(webhook.HeaderSignature, w.signatureHeader(ctx, delivery, secret, timestamp, body))
	headers.Set(webhook.HeaderAttempt, strconv.FormatInt(int64(delivery.Attempts), 10))
	headers.Set("Content-Type", "application/json")
	return headers
}

// send performs the single HTTP request of this attempt. The endpoint timeout
// (snapshotted on the delivery) can only shorten the client's global request
// timeout.
func (w *DeliveryWorker) send(ctx context.Context, delivery *repository.Delivery, headers http.Header, body []byte) sentRequest {
	reqCtx, cancel := context.WithTimeout(ctx, deliveryTimeout(delivery.TimeoutMs))
	defer cancel()
	started := time.Now()
	result, err := w.client.Do(reqCtx, delivery.Url, headers, body)
	return sentRequest{result: result, err: err, elapsed: time.Since(started)}
}

// sentRequest is what one HTTP request produced: a response or a transport
// error, and how long it took.
type sentRequest struct {
	result  *webhook.Result
	err     error
	elapsed time.Duration
}

// releaseInterrupted gives the attempt back instead of recording a failure:
// on the final attempt a failure would dead-letter a delivery that may never
// have reached the receiver. The request may still have arrived, which is
// within the at-least-once contract (receivers deduplicate). Returning the job
// context's error lets the jobs worker requeue the job without consuming its
// attempt when it is shutting down.
func (w *DeliveryWorker) releaseInterrupted(ctx context.Context, delivery *repository.Delivery, jobErr error) error {
	if err := w.store.ReleaseDeliveryClaim(ctx, delivery.ID, delivery.Attempts); err != nil {
		if !errors.Is(err, repository.ErrDeliveryLeaseLost) {
			return fmt.Errorf("webhooks: release interrupted delivery %d: %w", delivery.ID, err)
		}
		slog.Warn("webhooks: interrupted delivery claim was already superseded",
			slog.Int64("delivery_id", delivery.ID),
			slog.Int("attempt", int(delivery.Attempts)),
		)
	}
	return fmt.Errorf("webhooks: delivery %d interrupted before a response: %w", delivery.ID, jobErr)
}

// attemptResult is the classified outcome of one delivery attempt.
type attemptResult struct {
	outcome    webhook.Outcome
	statusCode int
	reason     string
	retryAfter string
	elapsed    time.Duration
}

// classifyAttempt maps a transport error or an HTTP response to an outcome.
func classifyAttempt(sent sentRequest) attemptResult {
	if sent.err != nil {
		return attemptResult{
			outcome: webhook.ClassifyError(sent.err),
			reason:  truncateError(fmt.Sprintf("request failed: %v", sent.err)),
			elapsed: sent.elapsed,
		}
	}
	result := sent.result
	attempt := attemptResult{
		outcome:    webhook.ClassifyStatus(result.StatusCode),
		statusCode: result.StatusCode,
		retryAfter: result.Header.Get("Retry-After"),
		elapsed:    sent.elapsed,
	}
	if attempt.outcome != webhook.OutcomeSuccess {
		attempt.reason = webhook.StatusError(result.StatusCode)
	}
	return attempt
}

// recordAttempt appends the attempt to the delivery log before the delivery
// row changes state.
func (w *DeliveryWorker) recordAttempt(ctx context.Context, delivery *repository.Delivery, attempt attemptResult) error {
	if err := w.store.CreateDeliveryAttempt(ctx, repository.AttemptDraft{
		DeliveryID: delivery.ID,
		Attempt:    delivery.Attempts,
		StatusCode: attempt.statusCode,
		Duration:   attempt.elapsed,
		Error:      attempt.reason,
	}); err != nil {
		return fmt.Errorf("webhooks: record delivery attempt: %w", err)
	}
	return nil
}

// finish applies the attempt outcome to the delivery row: succeeded, dead, or
// rescheduled with Retry-After/backoff while attempts remain.
func (w *DeliveryWorker) finish(ctx context.Context, delivery *repository.Delivery, attempt attemptResult) error {
	statusCode, reason := attempt.statusCode, attempt.reason
	switch attempt.outcome {
	case webhook.OutcomeSuccess:
		return w.fenced(delivery, "mark delivery succeeded",
			w.store.MarkDeliverySucceeded(ctx, delivery.ID, delivery.Attempts, statusCode))

	case webhook.OutcomePermanent:
		return w.fenced(delivery, "mark delivery dead",
			w.store.MarkDeliveryDead(ctx, delivery.ID, delivery.Attempts, statusCode, reason))

	default:
		if delivery.Attempts >= delivery.MaxAttempts {
			return w.fenced(delivery, "mark delivery dead after retries",
				w.store.MarkDeliveryDead(ctx, delivery.ID, delivery.Attempts, statusCode, reason))
		}
		now := w.now()
		delay, _ := webhook.RetryDelay(int(delivery.Attempts), w.opts.RetryBase, w.opts.RetryMax, attempt.retryAfter, now)
		if delay < 0 {
			delay = 0
		}
		next := now.Add(delay)
		return w.fenced(delivery, "reschedule delivery", w.store.RescheduleDeliveryWithJob(ctx, repository.RetryDraft{
			DeliveryID:    delivery.ID,
			Attempt:       delivery.Attempts,
			NextAttemptAt: next,
			StatusCode:    statusCode,
			Error:         reason,
		}))
	}
}

// fenced wraps an outcome write. ErrDeliveryLeaseLost means this worker's
// claim lease expired and a newer attempt (or a terminal state) owns the row;
// the stale result is discarded with a warning rather than retried, because a
// retry would hit the same fence forever.
func (w *DeliveryWorker) fenced(delivery *repository.Delivery, action string, err error) error {
	if errors.Is(err, repository.ErrDeliveryLeaseLost) {
		slog.Warn("webhooks: discarded stale delivery outcome; claim lease lost",
			slog.Int64("delivery_id", delivery.ID),
			slog.Int("attempt", int(delivery.Attempts)),
			slog.String("action", action),
		)
		return nil
	}
	if err != nil {
		return fmt.Errorf("webhooks: %s: %w", action, err)
	}
	return nil
}

// signatureHeader signs with the delivery's snapshot secret. When the endpoint
// secret was rotated after the snapshot, it also signs with the current secret
// during secretRotationGrace (receivers accept either "v1=" value), and with
// the current secret alone after it. A failed lookup falls back to the
// snapshot signature, which is what the delivery was created with.
func (w *DeliveryWorker) signatureHeader(ctx context.Context, delivery *repository.Delivery, snapshot []byte, timestamp time.Time, body []byte) string {
	sign := func(secret []byte) string {
		return webhook.SignatureHeader(webhook.Sign(secret, delivery.EventID, delivery.EventType, timestamp, body))
	}
	current, err := w.store.ActiveSigningSecret(ctx, delivery.EndpointID)
	if err != nil {
		slog.Error("webhooks: load current endpoint secret; signing with the snapshot only",
			slog.Int64("delivery_id", delivery.ID),
			slog.Int64("endpoint_id", delivery.EndpointID),
			slog.Any("error", err),
		)
		return sign(snapshot)
	}
	// Each rotation seals a fresh secret with a random nonce, so an identical
	// ciphertext means the snapshot is still the active secret.
	if current.Ciphertext == delivery.SecretCiphertext {
		return sign(snapshot)
	}
	currentSecret, err := w.codec.Open(current.Ciphertext)
	if err != nil {
		slog.Error("webhooks: current endpoint secret could not be decrypted; signing with the snapshot only",
			slog.Int64("delivery_id", delivery.ID),
			slog.Int64("endpoint_id", delivery.EndpointID),
			slog.Any("error", err),
		)
		return sign(snapshot)
	}
	if w.now().Before(current.CreatedAt.Add(secretRotationGrace)) {
		return sign(snapshot) + "," + sign(currentSecret)
	}
	return sign(currentSecret)
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
