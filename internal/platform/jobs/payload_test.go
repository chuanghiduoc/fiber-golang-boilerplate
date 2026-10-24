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

func TestPayloadCodecOpenCaseVariantEnvelope(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	plaintext := []byte(`{"token":"secret-token"}`)
	sealed, err := codec.Seal(plaintext)
	require.NoError(t, err)

	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(sealed, &envelope))
	mixedCase, err := json.Marshal(map[string]any{
		"V":     envelope.Version,
		"Nonce": envelope.Nonce,
		"Data":  envelope.Data,
	})
	require.NoError(t, err)
	assert.True(t, payloadLooksEncrypted(mixedCase))

	opened, err := codec.Open(mixedCase)
	require.NoError(t, err)
	assert.Equal(t, plaintext, opened)
}

func TestPayloadCodecRejectsEnvelopeWithoutAAD(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	nonce := make([]byte, codec.aead.NonceSize())
	ciphertext := codec.aead.Seal(nil, nonce, []byte(`{"token":"secret-token"}`), nil)
	envelope, err := json.Marshal(payloadEnvelope{
		Version: payloadEnvelopeVersion,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(ciphertext),
	})
	require.NoError(t, err)

	_, err = codec.Open(envelope)
	require.ErrorIs(t, err, ErrPayloadDecryption,
		"an envelope sealed without the version/context AAD must not authenticate")
}

func TestPayloadLooksEncrypted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "version", payload: `{"v":1}`, want: true},
		{name: "nonce only", payload: `{"nonce":"abc"}`, want: true},
		{name: "data only", payload: `{"data":"abc"}`, want: true},
		{name: "mixed case version and data", payload: `{"V":1,"Data":"abc"}`, want: true},
		{name: "mixed case nonce", payload: `{"Nonce":"abc"}`, want: true},
		{name: "plaintext email", payload: `{"To":["user@example.com"],"Subject":"Hello","Body":"body"}`, want: false},
		{name: "empty object", payload: `{}`, want: false},
		{name: "array", payload: `[1,2,3]`, want: false},
		{name: "null", payload: `null`, want: false},
		{name: "invalid JSON", payload: `not-json`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, payloadLooksEncrypted(json.RawMessage(tt.payload)))
		})
	}
}

func TestPayloadCodecErrorsDoNotLeakPlaintext(t *testing.T) {
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

	payloads := []json.RawMessage{
		tampered,
		json.RawMessage(`{"v":"secret-token"}`),
		json.RawMessage(`{"nonce":"secret-token"}`),
		json.RawMessage(`{"v":1,"nonce":"secret-token","data":"secret-token"}`),
		json.RawMessage(`secret-token`),
	}

	for _, payload := range payloads {
		_, err := codec.Open(payload)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret-token",
			"a payload error must not echo plaintext or secret material")
	}
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
		{name: "nonce only", payload: `{"nonce":"` + validNonce + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "mixed case envelope missing nonce", payload: `{"V":1,"Data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
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
