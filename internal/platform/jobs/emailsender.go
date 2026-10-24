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
	codec *PayloadCodec
}

// QueuedSenderOption configures a QueuedSender.
type QueuedSenderOption func(*QueuedSender)

// WithPayloadCodec encrypts email job payloads at rest. Without a codec the
// payload is stored as plaintext JSON, preserving the previous behavior.
func WithPayloadCodec(c *PayloadCodec) QueuedSenderOption {
	return func(s *QueuedSender) {
		s.codec = c
	}
}

func NewQueuedSender(queue *Queue, opts ...QueuedSenderOption) *QueuedSender {
	sender := &QueuedSender{queue: queue}
	for _, opt := range opts {
		if opt != nil {
			opt(sender)
		}
	}
	return sender
}

func (s *QueuedSender) Send(ctx context.Context, msg email.Message) error {
	if s == nil || s.queue == nil {
		return fmt.Errorf("jobs: email queue is nil")
	}
	job, err := emailJob(msg, s.codec)
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
	job, err := emailJob(msg, s.codec)
	if err != nil {
		return err
	}
	if _, err := s.queue.EnqueueTx(ctx, tx, job); err != nil {
		return fmt.Errorf("jobs: enqueue email: %w", err)
	}
	return nil
}

// emailJob marshals one email message into the job payload and seals it when a
// codec is configured. Queue.Enqueue and Queue.EnqueueTx apply the defaults and
// validate the job so both paths stay identical.
func emailJob(msg email.Message, codec *PayloadCodec) (Job, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: encode email: %w", err)
	}
	if codec != nil {
		payload, err = codec.Seal(payload)
		if err != nil {
			return Job{}, fmt.Errorf("jobs: encrypt email payload: %w", err)
		}
	}
	return Job{
		Kind:        EmailJobKind,
		Payload:     payload,
		MaxAttempts: 10,
	}, nil
}

// emailHandlerConfig holds the optional EmailHandler settings.
type emailHandlerConfig struct {
	codec *PayloadCodec
}

// EmailHandlerOption configures an EmailHandler.
type EmailHandlerOption func(*emailHandlerConfig)

// WithEmailPayloadCodec opens encrypted email payloads before decoding them.
// Plaintext payloads from before encryption was enabled still decode.
func WithEmailPayloadCodec(c *PayloadCodec) EmailHandlerOption {
	return func(cfg *emailHandlerConfig) {
		cfg.codec = c
	}
}

// EmailHandler adapts a concrete email.Sender to a jobs.Handler.
func EmailHandler(sender email.Sender, opts ...EmailHandlerOption) Handler {
	cfg := emailHandlerConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	return func(ctx context.Context, payload json.RawMessage) error {
		decoded := []byte(payload)
		if cfg.codec != nil {
			var err error
			decoded, err = cfg.codec.Open(payload)
			if err != nil {
				return fmt.Errorf("jobs: open email payload: %w", err)
			}
		}

		var msg email.Message
		if err := json.Unmarshal(decoded, &msg); err != nil {
			return fmt.Errorf("jobs: decode email payload: %w", err)
		}
		return sender.Send(ctx, msg)
	}
}
