package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// EmailJobKind is the job kind registered for durable email delivery.
const EmailJobKind = "email.send"

// QueuedSender enqueues emails instead of sending them inline. A Worker
// registered for EmailJobKind performs the actual SMTP/console send with
// retries and dead-letter handling.
type QueuedSender struct {
	queue *Queue
}

func NewQueuedSender(queue *Queue) *QueuedSender {
	return &QueuedSender{queue: queue}
}

func (s *QueuedSender) Send(ctx context.Context, msg email.Message) error {
	if s == nil || s.queue == nil {
		return fmt.Errorf("jobs: email queue is nil")
	}
	return s.enqueue(ctx, s.queue.pool, msg)
}

// SendTx enqueues msg using tx, so the email job commits atomically with the
// caller's transaction (for example, the token row it belongs to).
func (s *QueuedSender) SendTx(ctx context.Context, tx pgx.Tx, msg email.Message) error {
	if s == nil || s.queue == nil {
		return fmt.Errorf("jobs: email queue is nil")
	}
	if tx == nil {
		return ErrNoTx
	}
	return s.enqueue(ctx, tx, msg)
}

// enqueue marshals one email into a job and inserts it through db, which is
// either the queue's pool or a caller-owned transaction.
func (s *QueuedSender) enqueue(ctx context.Context, db queryRower, msg email.Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jobs: encode email: %w", err)
	}
	job, err := Job{
		Kind:        EmailJobKind,
		Payload:     payload,
		MaxAttempts: 10,
	}.normalize(time.Now())
	if err != nil {
		return fmt.Errorf("jobs: prepare email job: %w", err)
	}
	if db == nil {
		return ErrNoPool
	}
	if _, err := insertJob(ctx, db, job); err != nil {
		return fmt.Errorf("jobs: enqueue email: %w", err)
	}
	return nil
}

// EmailHandler adapts a concrete email.Sender to a jobs.Handler.
func EmailHandler(sender email.Sender) Handler {
	return func(ctx context.Context, payload json.RawMessage) error {
		var msg email.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("jobs: decode email payload: %w", err)
		}
		return sender.Send(ctx, msg)
	}
}
