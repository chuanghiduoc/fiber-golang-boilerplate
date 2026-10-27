package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
)

// EncryptedEmailJobKind is the only email job kind: its payload is a
// kid-bearing sealed envelope, so a queued message never holds plaintext PII
// or links.
const EncryptedEmailJobKind = "email.send.encrypted"

var (
	// ErrEmailNoRecipients is returned when a queued email has no recipients.
	// The guard prevents a malformed payload from being sent as an empty
	// message.
	ErrEmailNoRecipients = errors.New("jobs: email has no recipients")
	// ErrPayloadCodecRequired is returned when an email job is built or opened
	// without a payload codec.
	ErrPayloadCodecRequired = errors.New("jobs: email jobs require a payload codec")
)

// QueuedSender enqueues emails instead of sending them inline. A Worker
// registered for EncryptedEmailJobKind performs the actual SMTP/console send
// with retries and dead-letter handling.
type QueuedSender struct {
	queue *Queue
	codec *PayloadCodec
}

// NewQueuedSender builds a sender that seals every message with codec. A nil
// codec is rejected when a message is enqueued.
func NewQueuedSender(queue *Queue, codec *PayloadCodec) *QueuedSender {
	return &QueuedSender{queue: queue, codec: codec}
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

// emailJob marshals one email message into the job payload and seals it. Queue
// applies the defaults and validates the job.
func emailJob(msg email.Message, codec *PayloadCodec) (Job, error) {
	if codec == nil {
		return Job{}, ErrPayloadCodecRequired
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: encode email: %w", err)
	}
	sealed, err := codec.Seal(payload)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: encrypt email payload: %w", err)
	}
	return Job{
		Kind:        EncryptedEmailJobKind,
		Payload:     sealed,
		MaxAttempts: 10,
	}, nil
}

// EmailHandler adapts a concrete email.Sender to a jobs.Handler. The codec is
// required: every email job carries a sealed envelope.
func EmailHandler(sender email.Sender, codec *PayloadCodec) Handler {
	// A malformed envelope, undecodable JSON, or a message without recipients
	// is a property of the job and can never succeed, so it dead-letters
	// immediately. Decryption failures stay retryable: a worker may be missing
	// a key another replica already seals with until the rollout completes.
	return func(ctx context.Context, payload json.RawMessage) error {
		if codec == nil {
			return Permanent(ErrPayloadCodecRequired)
		}
		decoded, err := codec.Open(payload)
		if errors.Is(err, ErrInvalidPayloadEnvelope) {
			return Permanent(fmt.Errorf("jobs: open email payload: %w", err))
		}
		if err != nil {
			return fmt.Errorf("jobs: open email payload: %w", err)
		}

		var msg email.Message
		if err := json.Unmarshal(decoded, &msg); err != nil {
			return Permanent(fmt.Errorf("jobs: decode email payload: %w", err))
		}
		if len(msg.To) == 0 {
			return Permanent(ErrEmailNoRecipients)
		}
		return sender.Send(ctx, msg)
	}
}
