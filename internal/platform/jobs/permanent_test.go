package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

func TestPermanentWrapsAndClassifies(t *testing.T) {
	t.Parallel()

	assert.NoError(t, Permanent(nil))

	cause := errors.New("bad payload")
	err := fmt.Errorf("handler: %w", Permanent(cause))
	assert.True(t, IsPermanent(err), "a wrapped PermanentError is still permanent")
	assert.ErrorIs(t, err, cause, "errors.Is must see the cause")
	assert.Contains(t, err.Error(), "permanent failure")
	assert.False(t, IsPermanent(cause))
}

func TestProcessJobDeadLettersPermanentFailureImmediately(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake
	worker.Register("bad", func(context.Context, json.RawMessage) error {
		return Permanent(errors.New("payload does not decode"))
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          11,
		Kind:        "bad",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 10,
	}))

	require.Len(t, fake.failures, 1)
	assert.Nil(t, fake.failures[0].retryAt, "a permanent failure must be dead-lettered on its first attempt")
	assert.Equal(t, "permanent failure (not retried): payload does not decode", fake.failures[0].reason)
}

type countingSender struct{ sent int }

func (s *countingSender) Send(context.Context, email.Message) error {
	s.sent++
	return nil
}

func TestEmailHandlerPermanentAndRetryableFailures(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	otherKey, err := NewPayloadCodec("another-data-encryption-key-32-chars-x")
	require.NoError(t, err)
	foreign, err := otherKey.Seal([]byte(`{"To":["a@example.com"]}`))
	require.NoError(t, err)

	tests := []struct {
		name      string
		payload   string
		permanent bool
	}{
		{name: "malformed envelope", payload: `{"v":1,"nonce":"***","data":"x"}`, permanent: true},
		{name: "undecodable message", payload: `{"To":"not-a-list"}`, permanent: true},
		{name: "no recipients", payload: `{"Subject":"hi"}`, permanent: true},
		// The sealing key may still be rolling out to this worker.
		{name: "unknown key", payload: string(foreign), permanent: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sender := &countingSender{}
			handler := EmailHandler(sender, WithEmailPayloadCodec(codec))
			err := handler(context.Background(), json.RawMessage(tt.payload))
			require.Error(t, err)
			assert.Equal(t, tt.permanent, IsPermanent(err), "error: %v", err)
			assert.Zero(t, sender.sent)
		})
	}
}
