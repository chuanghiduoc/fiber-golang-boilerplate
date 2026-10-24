package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

type recordingEmailSender struct {
	messages []email.Message
	err      error
}

func (s *recordingEmailSender) Send(_ context.Context, msg email.Message) error {
	s.messages = append(s.messages, msg)
	return s.err
}

func testEmailMessage() email.Message {
	return email.Message{
		To:      []string{"user@example.com"},
		Subject: "Reset your password",
		Body:    "Reset your password: https://app.example/reset?token=secret-token",
		HTML:    `<a href="https://app.example/reset?token=secret-token">Reset</a>`,
	}
}

func TestEmailJobWithoutCodecKeepsPlaintext(t *testing.T) {
	t.Parallel()

	msg := testEmailMessage()
	job, err := emailJob(msg, nil)
	require.NoError(t, err)
	require.Equal(t, EmailJobKind, job.Kind)
	assert.Contains(t, string(job.Payload), "secret-token")

	var decoded email.Message
	require.NoError(t, json.Unmarshal(job.Payload, &decoded))
	assert.Equal(t, msg, decoded)
}

func TestEmailJobWithCodecSealsPayload(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	msg := testEmailMessage()
	job, err := emailJob(msg, codec)
	require.NoError(t, err)
	require.Equal(t, EmailJobKind, job.Kind)

	encoded := string(job.Payload)
	assert.NotContains(t, encoded, "secret-token")
	assert.NotContains(t, encoded, "user@example.com")

	opened, err := codec.Open(job.Payload)
	require.NoError(t, err)
	var decoded email.Message
	require.NoError(t, json.Unmarshal(opened, &decoded))
	assert.Equal(t, msg, decoded)
}

func TestEmailHandlerWithCodecDecodesEncryptedPayload(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	msg := testEmailMessage()
	job, err := emailJob(msg, codec)
	require.NoError(t, err)

	sender := &recordingEmailSender{}
	handler := EmailHandler(sender, WithEmailPayloadCodec(codec))
	require.NoError(t, handler(context.Background(), job.Payload))
	require.Len(t, sender.messages, 1)
	assert.Equal(t, msg, sender.messages[0])
}

func TestEmailHandlerWithCodecAcceptsLegacyPlaintext(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	msg := testEmailMessage()
	payload, err := json.Marshal(msg)
	require.NoError(t, err)

	sender := &recordingEmailSender{}
	handler := EmailHandler(sender, WithEmailPayloadCodec(codec))
	require.NoError(t, handler(context.Background(), payload))
	require.Len(t, sender.messages, 1)
	assert.Equal(t, msg, sender.messages[0])
}

func TestEmailHandlerWithoutCodecDecodesPlaintext(t *testing.T) {
	t.Parallel()

	msg := testEmailMessage()
	payload, err := json.Marshal(msg)
	require.NoError(t, err)

	sender := &recordingEmailSender{}
	require.NoError(t, EmailHandler(sender)(context.Background(), payload))
	require.Len(t, sender.messages, 1)
	assert.Equal(t, msg, sender.messages[0])
}

func TestEmailHandlerWithCodecReturnsErrorForTamperedPayload(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	msg := testEmailMessage()
	job, err := emailJob(msg, codec)
	require.NoError(t, err)

	// Break the ciphertext while keeping the envelope structurally valid.
	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(job.Payload, &envelope))
	envelope.Data = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tampered, err := json.Marshal(envelope)
	require.NoError(t, err)

	sender := &recordingEmailSender{}
	handler := EmailHandler(sender, WithEmailPayloadCodec(codec))
	err = handler(context.Background(), tampered)
	require.ErrorIs(t, err, ErrPayloadDecryption)
	assert.Empty(t, sender.messages, "a tampered payload must not reach the sender")
}

func TestEmailHandlerWithoutCodecRejectsEncryptedPayload(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	msg := testEmailMessage()
	job, err := emailJob(msg, codec)
	require.NoError(t, err)

	sender := &recordingEmailSender{}
	err = EmailHandler(sender)(context.Background(), job.Payload)
	require.ErrorIs(t, err, ErrEncryptedPayloadWithoutCodec)
	assert.Empty(t, sender.messages, "an encrypted payload must not be sent as an empty email")
}

func TestEmailHandlerWithoutCodecRejectsEnvelopeLikePayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload json.RawMessage
	}{
		{name: "nonce only", payload: json.RawMessage(`{"nonce":"abc"}`)},
		{name: "mixed case", payload: json.RawMessage(`{"V":1,"Data":"abc"}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sender := &recordingEmailSender{}
			err := EmailHandler(sender)(context.Background(), tt.payload)
			require.ErrorIs(t, err, ErrEncryptedPayloadWithoutCodec)
			assert.Empty(t, sender.messages)
		})
	}
}

func TestEmailHandlerWithWrongCodecFails(t *testing.T) {
	t.Parallel()

	sealer, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	opener, err := NewPayloadCodec("a-different-jwt-secret-at-least-32-characters")
	require.NoError(t, err)

	job, err := emailJob(testEmailMessage(), sealer)
	require.NoError(t, err)

	sender := &recordingEmailSender{}
	err = EmailHandler(sender, WithEmailPayloadCodec(opener))(context.Background(), job.Payload)
	require.ErrorIs(t, err, ErrPayloadDecryption)
	assert.Empty(t, sender.messages)
}

func TestEmailHandlerRejectsZeroRecipients(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	tests := []struct {
		name  string
		codec *PayloadCodec
	}{
		{name: "plaintext", codec: nil},
		{name: "encrypted", codec: codec},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := email.Message{Subject: "No recipients"}
			job, err := emailJob(msg, tt.codec)
			require.NoError(t, err)

			sender := &recordingEmailSender{}
			handler := EmailHandler(sender, WithEmailPayloadCodec(tt.codec))
			err = handler(context.Background(), job.Payload)
			require.ErrorIs(t, err, ErrEmailNoRecipients)
			assert.Empty(t, sender.messages)
		})
	}
}

func TestEmailHandlerPropagatesSenderError(t *testing.T) {
	t.Parallel()

	msg := testEmailMessage()
	payload, err := json.Marshal(msg)
	require.NoError(t, err)

	sendErr := errors.New("smtp unavailable")
	sender := &recordingEmailSender{err: sendErr}
	require.ErrorIs(t, EmailHandler(sender)(context.Background(), payload), sendErr)
}
