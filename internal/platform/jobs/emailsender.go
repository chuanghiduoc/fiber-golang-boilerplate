package jobs

import (
	"context"
	"encoding/json"
	"fmt"

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
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jobs: encode email: %w", err)
	}
	_, err = s.queue.Enqueue(ctx, Job{
		Kind:        EmailJobKind,
		Payload:     payload,
		MaxAttempts: 10,
	})
	if err != nil {
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
