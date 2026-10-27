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

func TestPayloadCodecRejectsPlaintext(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)

	// A plaintext payload is no longer a job body: every job payload is an
	// encrypted envelope.
	payload := json.RawMessage(`{"To":["user@example.com"],"Subject":"Hello","Body":"body"}`)
	_, err = codec.Open(payload)
	require.ErrorIs(t, err, ErrInvalidPayloadEnvelope)

	nonObject := json.RawMessage(`[1,2,3]`)
	_, err = codec.Open(nonObject)
	require.ErrorIs(t, err, ErrInvalidPayloadEnvelope)
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
		"Kid":   envelope.KeyID,
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

	key := codec.ring.Active()
	nonce := make([]byte, key.AEAD.NonceSize())
	ciphertext := key.AEAD.Seal(nil, nonce, []byte(`{"token":"secret-token"}`), nil)
	envelope := payloadEnvelope{Version: payloadEnvelopeVersion, KeyID: key.ID}
	envelope.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	envelope.Data = base64.RawURLEncoding.EncodeToString(ciphertext)
	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)

	_, err = codec.Open(encoded)
	require.ErrorIs(t, err, ErrPayloadDecryption,
		"an envelope sealed without the version/context AAD must not authenticate")
}

// sealLegacyPayload builds a version 1 envelope (no key id) the way retired
// codecs sealed payloads.
func sealLegacyPayload(t *testing.T, codec *PayloadCodec, plaintext []byte) json.RawMessage {
	t.Helper()
	aead := codec.ring.Active().AEAD
	nonce := make([]byte, aead.NonceSize())
	envelope, err := json.Marshal(payloadEnvelope{
		Version: 1,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, plaintext, []byte(payloadKeyInfo))),
	})
	require.NoError(t, err)
	return envelope
}

func TestPayloadCodecSealsWithActiveKeyID(t *testing.T) {
	t.Parallel()

	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	sealed, err := codec.Seal([]byte(`{}`))
	require.NoError(t, err)

	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(sealed, &envelope))
	assert.Equal(t, payloadEnvelopeVersion, envelope.Version)
	assert.Equal(t, codec.ring.Active().ID, envelope.KeyID)
	assert.Len(t, envelope.KeyID, 8)
}

func TestPayloadCodecRejectsLegacyEnvelope(t *testing.T) {
	t.Parallel()

	plaintext := []byte(`{"token":"legacy-token"}`)
	codec, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	legacy := sealLegacyPayload(t, codec, plaintext)

	_, err = codec.Open(legacy)
	require.ErrorIs(t, err, ErrInvalidPayloadEnvelope, "a kid-less version 1 envelope is no longer accepted")
}

func TestPayloadCodecRotation(t *testing.T) {
	t.Parallel()

	const newKey = "new-data-encryption-key-at-least-32-chars"
	plaintext := []byte(`{"token":"secret-token"}`)
	old, err := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, err)
	sealedOld, err := old.Seal(plaintext)
	require.NoError(t, err)

	rotated, err := NewPayloadCodec(newKey, testPayloadSecret)
	require.NoError(t, err)
	opened, err := rotated.Open(sealedOld)
	require.NoError(t, err, "a previous key must open envelopes it sealed")
	assert.Equal(t, plaintext, opened)

	sealedNew, err := rotated.Seal(plaintext)
	require.NoError(t, err)
	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(sealedNew, &envelope))
	assert.Equal(t, rotated.ring.Active().ID, envelope.KeyID, "new envelopes use the active key")

	// A worker that does not know the new key yet cannot open it.
	_, err = old.Open(sealedNew)
	require.ErrorIs(t, err, ErrPayloadDecryption)
	assert.Contains(t, err.Error(), envelope.KeyID)
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

	validNonce := base64.RawURLEncoding.EncodeToString(make([]byte, codec.ring.Active().AEAD.NonceSize()))
	validData := base64.RawURLEncoding.EncodeToString([]byte("not-really-ciphertext"))
	paddedData := base64.URLEncoding.EncodeToString([]byte("not-really-ciphertext!"))
	kid := codec.ring.Active().ID

	tests := []struct {
		name    string
		payload string
		wantErr error
	}{
		{name: "not JSON", payload: `not-json`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "truncated JSON", payload: `{"v":2`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "plaintext object", payload: `{"To":["a@example.com"]}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "missing data", payload: `{"v":2,"kid":"` + kid + `","nonce":"` + validNonce + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "missing nonce", payload: `{"v":2,"kid":"` + kid + `","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "missing kid", payload: `{"v":2,"nonce":"` + validNonce + `","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "unsupported version", payload: `{"v":1,"nonce":"` + validNonce + `","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "mixed case envelope missing nonce", payload: `{"V":2,"Kid":"` + kid + `","Data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "invalid nonce encoding", payload: `{"v":2,"kid":"` + kid + `","nonce":"***","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "padded data encoding", payload: `{"v":2,"kid":"` + kid + `","nonce":"` + validNonce + `","data":"` + paddedData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "invalid nonce length", payload: `{"v":2,"kid":"` + kid + `","nonce":"c2hvcnQ","data":"` + validData + `"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "invalid data encoding", payload: `{"v":2,"kid":"` + kid + `","nonce":"` + validNonce + `","data":"***"}`, wantErr: ErrInvalidPayloadEnvelope},
		{name: "authentication failure", payload: `{"v":2,"kid":"` + kid + `","nonce":"` + validNonce + `","data":"` + validData + `"}`, wantErr: ErrPayloadDecryption},
		{name: "unknown key", payload: `{"v":2,"kid":"unknown-key","nonce":"` + validNonce + `","data":"` + validData + `"}`, wantErr: ErrPayloadDecryption},
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
