package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const (
	defaultLease = 3 * time.Minute
	maxErrorLen  = 1000
	// bookkeepingTimeout bounds the detached writes that record an attempt
	// after the HTTP request was sent. Each write gets a fresh timeout; it must
	// not start before a slow receiver has responded.
	defaultBookkeepingTimeout = 5 * time.Second
)

// WorkerOptions configures retry and claim behavior.
type WorkerOptions struct {
	RetryBase time.Duration
	RetryMax  time.Duration
	Lease     time.Duration
}

// WorkerStore is the narrow persistence surface for delivery attempts.
type WorkerStore interface {
	ClaimDelivery(ctx context.Context, id uuid.UUID, lease time.Duration) (*Delivery, error)
	GetDelivery(ctx context.Context, id uuid.UUID) (*Delivery, error)
	MarkDeliverySucceeded(ctx context.Context, id uuid.UUID, attempt int32, statusCode int) error
	MarkDeliveryDead(ctx context.Context, id uuid.UUID, attempt int32, statusCode int, reason string) error
	ReleaseDeliveryClaim(ctx context.Context, id uuid.UUID, attempt int32) error
	SigningSecrets(ctx context.Context, endpointID uuid.UUID) (*SigningSecrets, error)
	// SnapshotSecret reads the sealed secret row a delivery references. It is
	// the delivery's snapshot; the worker never trusts a ciphertext copied
	// onto the delivery row.
	SnapshotSecret(ctx context.Context, secretID uuid.UUID) (string, error)
	ExpireUnclaimableDelivery(ctx context.Context, id uuid.UUID) (bool, error)
	RescheduleDeliveryWithJob(ctx context.Context, draft RetryDraft) error
	CreateDeliveryAttempt(ctx context.Context, draft AttemptDraft) error
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
	// bookkeepingTimeout is a field so tests can shrink it without waiting five
	// seconds; production uses defaultBookkeepingTimeout.
	bookkeepingTimeout time.Duration
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
		store:              store,
		codec:              codec,
		client:             client,
		opts:               opts,
		now:                func() time.Time { return time.Now().UTC() },
		bookkeepingTimeout: defaultBookkeepingTimeout,
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
	var job DeliveryJobPayload
	if err := json.Unmarshal(payload, &job); err != nil {
		return jobs.Permanent(fmt.Errorf("webhooks: invalid delivery job payload: %w", err))
	}
	if job.DeliveryID == uuid.Nil {
		return jobs.Permanent(errors.New("webhooks: invalid delivery job payload: missing delivery id"))
	}

	delivery, err := w.store.ClaimDelivery(ctx, job.DeliveryID, w.opts.Lease)
	if errors.Is(err, ErrNotClaimable) {
		return w.handleNotClaimable(ctx, job.DeliveryID)
	}
	if err != nil {
		return fmt.Errorf("webhooks: claim delivery %s: %w", job.DeliveryID, err)
	}

	body, failure := w.prepare(ctx, delivery)
	if failure != nil {
		if ctx.Err() != nil {
			// The snapshot lookup was interrupted by worker shutdown or job
			// timeout. That is not a delivery failure: release the fenced
			// claim so the next worker can retry without spending an attempt.
			return w.releaseInterrupted(ctx, delivery, ctx.Err())
		}
		attempt := attemptResult{outcome: webhook.OutcomePermanent, reason: failure.reason}
		if failure.retryable {
			// A decryption failure usually means a replica during a key
			// rollout lacks the key yet, so retry with backoff (and respect
			// the attempt budget) instead of dead-lettering.
			attempt.outcome = webhook.OutcomeRetry
		}
		if err := w.recordAttempt(ctx, delivery, attempt); err != nil {
			return err
		}
		return w.finish(ctx, delivery, attempt)
	}

	headers, err := w.requestHeaders(ctx, delivery, body)
	if err != nil {
		if ctx.Err() != nil {
			// The worker is shutting down (or the job timed out) before the
			// request was built; give the attempt back instead of spending it.
			return w.releaseInterrupted(ctx, delivery, ctx.Err())
		}
		// A signing-secret lookup or current-key open failure is retryable:
		// falling back to the snapshot could keep signing with a secret whose
		// grace window already closed. The cause is logged, not stored in
		// last_error, which the delivery API exposes.
		attempt := attemptResult{outcome: webhook.OutcomeRetry, reason: "endpoint signing secret is unavailable"}
		if recordErr := w.recordAttempt(ctx, delivery, attempt); recordErr != nil {
			return recordErr
		}
		return w.finish(ctx, delivery, attempt)
	}
	sent := w.send(ctx, delivery, headers, body)
	// The job context itself ended (worker shutdown or job timeout), as
	// opposed to the per-endpoint request timeout, before any response
	// arrived. That says nothing about the receiver.
	interrupted := sent.err != nil && ctx.Err() != nil
	jobErr := ctx.Err()

	if interrupted {
		return w.releaseInterrupted(ctx, delivery, jobErr)
	}
	attempt := classifyAttempt(sent)
	if err := w.recordAttempt(ctx, delivery, attempt); err != nil {
		return err
	}
	return w.finish(ctx, delivery, attempt)
}

// bookkeepingContext detaches a write from job cancellation but starts a fresh
// timeout for it. Starting the deadline before prepare/send would let it expire
// while a slow receiver is still responding, losing the outcome of an attempt
// that may already have been delivered.
func (w *DeliveryWorker) bookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := w.bookkeepingTimeout
	if timeout <= 0 {
		timeout = defaultBookkeepingTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
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

// prepare validates the delivery's snapshot secret and builds the signed
// body. A non-nil failure says the request could not be built; retryable
// distinguishes a transient decryption or lookup gap (a replica during a key
// rollout) from a structurally invalid envelope or a stored value that is not
// a Standard Webhooks secret, which retrying cannot fix.
func (w *DeliveryWorker) prepare(ctx context.Context, delivery *Delivery) (body []byte, failure *prepareFailure) {
	ciphertext, err := w.store.SnapshotSecret(ctx, delivery.SecretID)
	if err != nil {
		slog.Error("webhooks: delivery snapshot secret is unavailable",
			slog.String("delivery_id", delivery.ID.String()),
			slog.String("secret_id", delivery.SecretID.String()),
			slog.Any("error", err),
		)
		return nil, &prepareFailure{reason: "secret snapshot could not be loaded", retryable: true}
	}
	_, err = w.signingKey(EndpointSecretBinding(delivery.TenantID, delivery.EndpointID), ciphertext)
	if err != nil {
		slog.Error("webhooks: delivery secret is unusable",
			slog.String("delivery_id", delivery.ID.String()),
			slog.Any("error", err),
		)
		switch {
		case errors.Is(err, webhook.ErrInvalidSecret):
			return nil, &prepareFailure{reason: "secret snapshot is not a Standard Webhooks secret"}
		case errors.Is(err, ErrInvalidSecretEnvelope):
			return nil, &prepareFailure{reason: "secret snapshot is not a valid envelope"}
		default:
			return nil, &prepareFailure{reason: "secret snapshot could not be decrypted", retryable: true}
		}
	}
	body, err = deliveryBody(delivery)
	if err != nil {
		return nil, &prepareFailure{reason: "delivery payload is not valid JSON"}
	}
	return body, nil
}

// prepareFailure is why one delivery attempt could not even be built.
type prepareFailure struct {
	reason    string
	retryable bool
}

// signingKey opens a sealed "whsec_" secret and decodes its HMAC key.
func (w *DeliveryWorker) signingKey(binding SecretBinding, ciphertext string) ([]byte, error) {
	secret, err := w.codec.Open(binding, ciphertext)
	if err != nil {
		return nil, err
	}
	return webhook.DecodeSecret(string(secret))
}

// deliveryBody rebuilds the event envelope stored in
// webhook_events. The event type and dataVersion travel in this signed body.
// RequestID is process-internal and stays empty, so it is never sent.
func deliveryBody(delivery *Delivery) ([]byte, error) {
	return json.Marshal(events.Event{
		ID:          delivery.EventID,
		Type:        delivery.EventType,
		DataVersion: int(delivery.DataVersion),
		OccurredAt:  delivery.OccurredAt.UTC(),
		TenantID:    delivery.TenantID,
		Actor:       events.Actor{Type: delivery.ActorType, ID: delivery.ActorID},
		Data:        json.RawMessage(delivery.Payload),
	})
}

// requestHeaders builds the Standard Webhooks headers: webhook-id is the event
// id (stable across retries, the receiver's deduplication key) and the
// signature covers "<id>.<timestamp>.<exact body sent>".
func (w *DeliveryWorker) requestHeaders(ctx context.Context, delivery *Delivery, body []byte) (http.Header, error) {
	timestamp := w.now()
	signature, err := w.signatureHeader(ctx, delivery, timestamp, body)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set(webhook.HeaderID, delivery.EventID.String())
	headers.Set(webhook.HeaderTimestamp, strconv.FormatInt(timestamp.Unix(), 10))
	headers.Set(webhook.HeaderSignature, signature)
	headers.Set("Content-Type", "application/json")
	return headers, nil
}

// send performs the single HTTP request of this attempt. The endpoint timeout
// (snapshotted on the delivery) can only shorten the client's global request
// timeout.
func (w *DeliveryWorker) send(ctx context.Context, delivery *Delivery, headers http.Header, body []byte) sentRequest {
	reqCtx, cancel := context.WithTimeout(ctx, deliveryTimeout(delivery.TimeoutMS))
	defer cancel()
	started := time.Now()
	result, err := w.client.Do(reqCtx, delivery.URL, headers, body)
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
func (w *DeliveryWorker) releaseInterrupted(ctx context.Context, delivery *Delivery, jobErr error) error {
	writeCtx, cancel := w.bookkeepingContext(ctx)
	defer cancel()
	if err := w.store.ReleaseDeliveryClaim(writeCtx, delivery.ID, delivery.Attempts); err != nil {
		if !errors.Is(err, ErrDeliveryLeaseLost) {
			return fmt.Errorf("webhooks: release interrupted delivery %s: %w", delivery.ID, err)
		}
		slog.Warn("webhooks: interrupted delivery claim was already superseded",
			slog.String("delivery_id", delivery.ID.String()),
			slog.Int("attempt", int(delivery.Attempts)),
		)
	}
	return fmt.Errorf("webhooks: delivery %s interrupted before a response: %w", delivery.ID, jobErr)
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
			reason:  transportErrorReason(sent.err),
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

// transportErrorReason builds the recorded error text for a failed request.
// *url.Error embeds the full request URL (which may carry receiver tokens in
// its path or query), so only its inner cause is kept.
func transportErrorReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	return truncateError(fmt.Sprintf("request failed: %v", err))
}

// recordAttempt appends the attempt to the delivery log before the delivery
// row changes state.
func (w *DeliveryWorker) recordAttempt(ctx context.Context, delivery *Delivery, attempt attemptResult) error {
	writeCtx, cancel := w.bookkeepingContext(ctx)
	defer cancel()
	if err := w.store.CreateDeliveryAttempt(writeCtx, AttemptDraft{
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
func (w *DeliveryWorker) finish(ctx context.Context, delivery *Delivery, attempt attemptResult) error {
	writeCtx, cancel := w.bookkeepingContext(ctx)
	defer cancel()
	statusCode, reason := attempt.statusCode, attempt.reason
	switch attempt.outcome {
	case webhook.OutcomeSuccess:
		return w.fenced(delivery, "mark delivery succeeded",
			w.store.MarkDeliverySucceeded(writeCtx, delivery.ID, delivery.Attempts, statusCode))

	case webhook.OutcomePermanent:
		return w.fenced(delivery, "mark delivery dead",
			w.store.MarkDeliveryDead(writeCtx, delivery.ID, delivery.Attempts, statusCode, reason))

	default:
		if delivery.Attempts >= delivery.MaxAttempts {
			return w.fenced(delivery, "mark delivery dead after retries",
				w.store.MarkDeliveryDead(writeCtx, delivery.ID, delivery.Attempts, statusCode, reason))
		}
		now := w.now()
		delay, _ := webhook.RetryDelay(int(delivery.Attempts), w.opts.RetryBase, w.opts.RetryMax, attempt.retryAfter, now)
		if delay < 0 {
			delay = 0
		}
		next := now.Add(delay)
		return w.fenced(delivery, "reschedule delivery", w.store.RescheduleDeliveryWithJob(writeCtx, RetryDraft{
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
func (w *DeliveryWorker) fenced(delivery *Delivery, action string, err error) error {
	if errors.Is(err, ErrDeliveryLeaseLost) {
		slog.Warn("webhooks: discarded stale delivery outcome; claim lease lost",
			slog.String("delivery_id", delivery.ID.String()),
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

// signatureHeader signs with the endpoint's current secret first and, while
// the immediately previous secret's explicit grace window is still open, with
// that one too as a second space-separated "v1," signature. Receivers accept
// either, so they can switch secrets at any point inside the 24h window; a
// delivery sent after it (or after a second rotation) carries only the
// current signature. A failed lookup or an unusable current secret is returned
// as an error so the attempt retries.
func (w *DeliveryWorker) signatureHeader(ctx context.Context, delivery *Delivery, timestamp time.Time, body []byte) (string, error) {
	sign := func(key []byte) string {
		return webhook.Sign(key, delivery.EventID.String(), timestamp, body)
	}
	secrets, err := w.store.SigningSecrets(ctx, delivery.EndpointID)
	if err != nil {
		slog.Error("webhooks: load endpoint signing secrets",
			slog.String("delivery_id", delivery.ID.String()),
			slog.String("endpoint_id", delivery.EndpointID.String()),
			slog.Any("error", err),
		)
		return "", fmt.Errorf("load signing secrets: %w", err)
	}
	binding := EndpointSecretBinding(delivery.TenantID, delivery.EndpointID)
	currentKey, err := w.signingKey(binding, secrets.CurrentCiphertext)
	if err != nil {
		slog.Error("webhooks: current endpoint secret is unusable",
			slog.String("delivery_id", delivery.ID.String()),
			slog.String("endpoint_id", delivery.EndpointID.String()),
			slog.Any("error", err),
		)
		return "", fmt.Errorf("open current signing secret: %w", err)
	}
	signatures := []string{sign(currentKey)}
	if secrets.PreviousCiphertext != "" && secrets.PreviousCiphertext != secrets.CurrentCiphertext {
		previousKey, err := w.signingKey(binding, secrets.PreviousCiphertext)
		if err != nil {
			slog.Error("webhooks: previous endpoint secret is unusable; co-signing without it",
				slog.String("delivery_id", delivery.ID.String()),
				slog.String("endpoint_id", delivery.EndpointID.String()),
				slog.Any("error", err),
			)
		} else {
			signatures = append(signatures, sign(previousKey))
		}
	}
	return webhook.SignatureHeader(signatures...), nil
}

// handleNotClaimable distinguishes an already-terminal delivery (nothing to
// do) from a pending delivery whose claim lease is still held. The latter
// returns an error so the platform job retries after the lease expires; that
// is the crash-recovery path after a process dies mid-attempt.
func (w *DeliveryWorker) handleNotClaimable(ctx context.Context, id uuid.UUID) error {
	delivery, err := w.store.GetDelivery(ctx, id)
	if err != nil {
		// A missing row may have been purged by retention; the job is done.
		if errors.Is(err, apperror.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("webhooks: load delivery %s: %w", id, err)
	}
	if delivery.Status != "pending" {
		return nil
	}
	// A pending delivery that can never be claimed again (endpoint disabled or
	// deleted, or every attempt consumed but the final bookkeeping lost) would
	// otherwise stay pending forever once this job dead-letters.
	expired, err := w.store.ExpireUnclaimableDelivery(ctx, id)
	if err != nil {
		return fmt.Errorf("webhooks: expire delivery %s: %w", id, err)
	}
	if expired {
		slog.Warn("webhooks: dead-lettered unclaimable delivery", slog.String("delivery_id", id.String()))
		return nil
	}
	return fmt.Errorf("webhooks: delivery %s claim is busy", id)
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
