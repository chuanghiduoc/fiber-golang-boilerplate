package jobs

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPayloadSecret = "test-jwt-secret-at-least-32-characters-long"

func TestPayloadCodecRoundTrip(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	plaintext := []byte(`{"To":["user@example.com"],"Subject":"Reset your password","Body":"https://app.example/reset?token=secret-token"}`)
	sealed, err := codec.Seal(plaintext)
	require.NoError(t, err)
	require.True(t, json.Valid(sealed))

	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(sealed, &envelope))
	assert.Equal(t, payloadEnvelopeVersion, envelope.Version)
	assert.NotEmpty(t, envelope.Nonce)
	assert.NotEmpty(t, envelope.Data)

	encoded := string(sealed)
	for _, secret := range []string{"secret-token", "user@example.com", "Reset your password"} {
		assert.NotContains(t, encoded, secret, "sealed payload must not leak plaintext")
	}

	opened, err := codec.Open(sealed)
	require.NoError(t, err)
	assert.Equal(t, plaintext, opened)

	second, err := codec.Seal(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, sealed, second, "each seal must use a fresh nonce")
}

func TestPayloadCodecWrongKeyFails(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	sealed, err := codec.Seal([]byte(`{"token":"secret-token"}`))
	require.NoError(t, err)

	other, err := NewPayloadCodec("a-different-jwt-secret-at-least-32-characters")
	require.NoError(t, err)
	_, err = other.Open(sealed)
	require.ErrorIs(t, err, ErrPayloadDecryption)
}

func TestPayloadCodecTamperedDataFails(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	sealed, err := codec.Seal([]byte(`{"token":"secret-token"}`))
	require.NoError(t, err)

	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(sealed, &envelope))
	data, err := base64.RawURLEncoding.DecodeString(envelope.Data)
	require.NoError(t, err)
	data[0] ^= 0xff
	envelope.Data = base64.RawURLEncoding.EncodeToString(data)

	tampered, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = codec.Open(tampered)
	require.ErrorIs(t, err, ErrPayloadDecryption)
}

func TestPayloadCodecTamperedNonceFails(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	sealed, err := codec.Seal([]byte(`{"token":"secret-token"}`))
	require.NoError(t, err)

	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(sealed, &envelope))
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	require.NoError(t, err)
	nonce[0] ^= 0xff
	envelope.Nonce = base64.RawURLEncoding.EncodeToString(nonce)

	tampered, err := json.Marshal(envelope)
	require.NoError(t, err)
	_, err = codec.Open(tampered)
	require.ErrorIs(t, err, ErrPayloadDecryption)
}

func TestPayloadCodecPlaintextFallback(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	payload := json.RawMessage(`{"To":["legacy@example.com"],"Subject":"Legacy job","Body":"https://app.example/reset?token=old-token"}`)
	opened, err := codec.Open(payload)
	require.NoError(t, err)
	assert.Equal(t, []byte(payload), opened, "pre-deployment plaintext jobs must pass through unchanged")

	// Valid JSON that is not an object cannot be an envelope either.
	nonObject := json.RawMessage(`[1,2,3]`)
	opened, err = codec.Open(nonObject)
	require.NoError(t, err)
	assert.Equal(t, []byte(nonObject), opened)
}

func TestNewPayloadCodecRejectsEmptySecret(t *testing.T) {
	t.Parallel()

	for _, secret := range []string{"", " ", "\t\n"} {
		_, err := NewPayloadCodec(secret)
		require.ErrorIs(t, err, ErrPayloadSecretRequired, "secret %q must be rejected", secret)
	}
}

func TestPayloadCodecRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	validNonce := base64.RawURLEncoding.EncodeToString(make([]byte, codec.aead.NonceSize()))
	validData := base64.RawURLEncoding.EncodeToString([]byte("not-really-ciphertext"))

	tests := []struct {
		name    string
		payload string
		wantErr error
	}{
		{name: "not JSON", payload: `not-json`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "truncated JSON", payload: `{"v":1`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "missing data", payload: `{"v":1,"nonce":"` + validNonce + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "missing nonce", payload: `{"v":1,"data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "unsupported version", payload: `{"v":2,"nonce":"` + validNonce + `","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "invalid nonce encoding", payload: `{"v":1,"nonce":"***","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "invalid nonce length", payload: `{"v":1,"nonce":"c2hvcnQ","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "invalid data encoding", payload: `{"v":1,"nonce":"` + validNonce + `","data":"***"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "authentication failure", payload: `{"v":1,"nonce":"` + validNonce + `","data":"` + validData + `"}`, wantErr: ErrPayloadDecryption},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := codec.Open(json.RawMessage(tt.payload))
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestPayloadCodecNilReceiver(t *testing.T) {
	t.Parallel()

	var codec *PayloadCodec
	_, err := codec.Seal([]byte(`{}`))
	require.ErrorIs(t, err, ErrNilPayloadCodec)
	_, err = codec.Open(json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrNilPayloadCodec)
}

func TestPayloadCodecDifferentSecretsProduceDifferentCiphertext(t *testing.T) {
	t.Parallel()

	plaintext := []byte(`{"token":"secret-token"}`)
	first, err := NewPayloadCodec(strings.Repeat("a", 32))
	require.NoError(t, err)
	second, err := NewPayloadCodec(strings.Repeat("b", 32))
	require.NoError(t, err)

	firstSealed, err := first.Seal(plaintext)
	require.NoError(t, err)
	secondSealed, err := second.Seal(plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, firstSealed, secondSealed)

	_, err = first.Open(secondSealed)
	require.ErrorIs(t, err, ErrPayloadDecryption)
}
