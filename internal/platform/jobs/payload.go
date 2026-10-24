package jobs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const (
	// payloadKeyInfo domain-separates the job payload key from other uses of
	// the application secret.
	payloadKeyInfo = "fiber-golang-boilerplate/jobs/payload/v1"
	// payloadEnvelopeVersion is the only supported envelope version.
	payloadEnvelopeVersion = 1
	// payloadKeySize is the AES-256 key size in bytes.
	payloadKeySize = 32
)

var (
	// ErrPayloadSecretRequired is returned when a payload codec is created
	// without a usable secret.
	ErrPayloadSecretRequired = errors.New("jobs: payload secret is required")
	// ErrNilPayloadCodec is returned when Seal or Open is called on a nil
	// codec.
	ErrNilPayloadCodec = errors.New("jobs: payload codec is nil")
	// ErrInvalidPayloadEnvelope is returned for a malformed or unsupported
	// encrypted payload envelope.
	ErrInvalidPayloadEnvelope = errors.New("jobs: invalid payload envelope")
	// ErrPayloadDecryption is returned when an envelope cannot be
	// authenticated with the codec's key.
	ErrPayloadDecryption = errors.New("jobs: payload decryption failed")
)

// PayloadCodec seals job payloads at rest with AES-256-GCM. The key is derived
// from an application secret with HKDF-SHA256, so payloads from a different
// secret (or a different domain) cannot be opened.
type PayloadCodec struct {
	aead cipher.AEAD
}

// payloadEnvelope is the JSON stored in jobs.payload for encrypted jobs.
type payloadEnvelope struct {
	Version int    `json:"v"`
	Nonce   string `json:"nonce"`
	Data    string `json:"data"`
}

// NewPayloadCodec derives a 32-byte AES key from secret with HKDF-SHA256 and
// returns a codec that seals payloads with AES-256-GCM. An empty or
// whitespace-only secret is rejected.
func NewPayloadCodec(secret string) (*PayloadCodec, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrPayloadSecretRequired
	}

	key := make([]byte, payloadKeySize)
	reader := hkdf.New(sha256.New, []byte(secret), nil, []byte(payloadKeyInfo))
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("jobs: derive payload key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("jobs: create payload cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("jobs: create payload GCM: %w", err)
	}

	return &PayloadCodec{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce and returns a versioned
// JSON envelope.
func (c *PayloadCodec) Seal(plaintext []byte) (json.RawMessage, error) {
	if c == nil || c.aead == nil {
		return nil, ErrNilPayloadCodec
	}

	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("jobs: generate payload nonce: %w", err)
	}

	sealed, err := json.Marshal(payloadEnvelope{
		Version: payloadEnvelopeVersion,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(c.aead.Seal(nil, nonce, plaintext, nil)),
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: encode payload envelope: %w", err)
	}
	return json.RawMessage(sealed), nil
}

// Open decrypts an encrypted payload envelope. Plaintext JSON payloads queued
// before encryption was enabled are returned unchanged, so deploying the codec
// does not strand in-flight jobs. Non-JSON payloads and malformed, unsupported,
// or unauthenticated envelopes return an error.
func (c *PayloadCodec) Open(payload json.RawMessage) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, ErrNilPayloadCodec
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: payload is not valid JSON", ErrInvalidPayloadEnvelope)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		// Valid JSON that is not an object cannot be an envelope; keep
		// treating it as legacy plaintext for backward compatibility.
		return payload, nil
	}
	_, hasVersion := fields["v"]
	_, hasData := fields["data"]
	if !hasVersion && !hasData {
		return payload, nil
	}

	var envelope payloadEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPayloadEnvelope, err)
	}
	if envelope.Version != payloadEnvelopeVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidPayloadEnvelope, envelope.Version)
	}

	nonce, err := decodePayloadBase64(envelope.Nonce)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid nonce encoding", ErrInvalidPayloadEnvelope)
	}
	if len(nonce) != c.aead.NonceSize() {
		return nil, fmt.Errorf("%w: invalid nonce length %d", ErrInvalidPayloadEnvelope, len(nonce))
	}

	data, err := decodePayloadBase64(envelope.Data)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid data encoding", ErrInvalidPayloadEnvelope)
	}
	if len(data) < c.aead.Overhead() {
		return nil, fmt.Errorf("%w: ciphertext is truncated", ErrInvalidPayloadEnvelope)
	}

	plaintext, err := c.aead.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPayloadDecryption, err)
	}
	return plaintext, nil
}

// decodePayloadBase64 accepts base64url with or without padding so envelopes
// produced by other encoders remain readable.
func decodePayloadBase64(value string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}
