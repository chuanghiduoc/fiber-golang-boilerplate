package jobs

import (
	"context"
	"encoding/json"
	"fmt"

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
	job, err := emailJob(msg)
	if err != nil {
		return err
	}
	if _, err := s.queue.Enqueue(ctx, job); err != nil {
		return fmt.Errorf("jobs: enqueue email: %w", err)
	}
	return nil
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
	job, err := emailJob(msg)
	if err != nil {
		return err
	}
	if _, err := s.queue.EnqueueTx(ctx, tx, job); err != nil {
		return fmt.Errorf("jobs: enqueue email: %w", err)
	}
	return nil
}

// emailJob marshals one email message into the job payload. Queue.Enqueue and
// Queue.EnqueueTx apply the defaults and validate the job so both paths stay
// identical.
func emailJob(msg email.Message) (Job, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: encode email: %w", err)
	}
	return Job{
		Kind:        EmailJobKind,
		Payload:     payload,
		MaxAttempts: 10,
	}, nil
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
