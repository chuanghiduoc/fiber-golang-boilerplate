package jobs

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/keyring"
)

const (
	// payloadKeyInfo domain-separates the job payload key from other uses of
	// the same secret. It is also the HKDF info, so changing it would make
	// every queued envelope unreadable.
	payloadKeyInfo = "fiber-golang-boilerplate/jobs/payload/v1"
	// legacyPayloadEnvelopeVersion envelopes carry no key id; Open tries every
	// configured key.
	legacyPayloadEnvelopeVersion = 1
	// payloadEnvelopeVersion is the version Seal writes: it records the id of
	// the sealing key and binds it into the AAD.
	payloadEnvelopeVersion = 2
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
	// ErrEncryptedPayloadWithoutCodec is returned when a handler encounters an
	// encrypted envelope but was not configured with a payload codec.
	ErrEncryptedPayloadWithoutCodec = errors.New("jobs: encrypted payload requires a payload codec")
	// ErrPayloadDecryption is returned when an envelope cannot be
	// authenticated with any of the codec's keys, including an envelope whose
	// key id is not configured (for example a worker missing a key that
	// another replica already seals with).
	ErrPayloadDecryption = errors.New("jobs: payload decryption failed")
)

// PayloadCodec seals job payloads at rest with AES-256-GCM. Keys are derived
// from configured secrets with HKDF-SHA256, so payloads from a different
// secret (or a different domain) cannot be opened. Seal uses the active key;
// Open also accepts the previous keys given to NewPayloadCodec, so the
// encryption key can rotate while sealed jobs are still queued.
type PayloadCodec struct {
	ring *keyring.Ring
}

// payloadEnvelope is the JSON stored in jobs.payload for encrypted jobs.
type payloadEnvelope struct {
	Version int    `json:"v"`
	KeyID   string `json:"kid,omitempty"`
	Nonce   string `json:"nonce"`
	Data    string `json:"data"`
}

// NewPayloadCodec derives the active AES key from secret and decryption-only
// keys from previous (DATA_ENCRYPTION_PREVIOUS_KEYS). An empty or
// whitespace-only active secret is rejected.
func NewPayloadCodec(secret string, previous ...string) (*PayloadCodec, error) {
	ring, err := keyring.New(payloadKeyInfo, secret, previous...)
	if errors.Is(err, keyring.ErrSecretRequired) {
		return nil, ErrPayloadSecretRequired
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: payload keys: %w", err)
	}
	return &PayloadCodec{ring: ring}, nil
}

// payloadAAD binds the envelope version and key id to the ciphertext. Legacy
// envelopes used the bare domain string.
func payloadAAD(version int, keyID string) []byte {
	if version == legacyPayloadEnvelopeVersion {
		return []byte(payloadKeyInfo)
	}
	return []byte(fmt.Sprintf("%s|v%d|%s", payloadKeyInfo, version, keyID))
}

// Seal encrypts plaintext with the active key and a fresh random nonce and
// returns a versioned JSON envelope naming the key.
func (c *PayloadCodec) Seal(plaintext []byte) (json.RawMessage, error) {
	if c == nil || c.ring == nil {
		return nil, ErrNilPayloadCodec
	}
	key := c.ring.Active()

	nonce := make([]byte, key.AEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("jobs: generate payload nonce: %w", err)
	}

	sealed, err := json.Marshal(payloadEnvelope{
		Version: payloadEnvelopeVersion,
		KeyID:   key.ID,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(key.AEAD.Seal(nil, nonce, plaintext, payloadAAD(payloadEnvelopeVersion, key.ID))),
	})
	if err != nil {
		return nil, fmt.Errorf("jobs: encode payload envelope: %w", err)
	}
	return json.RawMessage(sealed), nil
}

// Open decrypts an encrypted payload envelope. Plaintext JSON payloads queued
// before encryption was enabled are returned unchanged, so deploying the codec
// does not strand in-flight jobs. Version 2 envelopes open with the key their
// id names; version 1 envelopes (no id) try every configured key. Non-JSON
// payloads and malformed, unsupported, or unauthenticated envelopes return an
// error.
func (c *PayloadCodec) Open(payload json.RawMessage) ([]byte, error) {
	if c == nil || c.ring == nil {
		return nil, ErrNilPayloadCodec
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%w: payload is not valid JSON", ErrInvalidPayloadEnvelope)
	}
	if !payloadLooksEncrypted(payload) {
		return payload, nil
	}

	var envelope payloadEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPayloadEnvelope, err)
	}
	keys, err := c.envelopeKeys(envelope)
	if err != nil {
		return nil, err
	}
	nonce, data, err := decodeEnvelope(envelope, c.ring.Active().AEAD)
	if err != nil {
		return nil, err
	}

	aad := payloadAAD(envelope.Version, envelope.KeyID)
	var openErr error
	for _, aead := range keys {
		plaintext, err := aead.Open(nil, nonce, data, aad)
		if err == nil {
			return plaintext, nil
		}
		openErr = err
	}
	return nil, fmt.Errorf("%w: %v", ErrPayloadDecryption, openErr)
}

// envelopeKeys returns the keys that may open envelope.
func (c *PayloadCodec) envelopeKeys(envelope payloadEnvelope) ([]cipher.AEAD, error) {
	switch envelope.Version {
	case legacyPayloadEnvelopeVersion:
		keys := make([]cipher.AEAD, 0, len(c.ring.Keys()))
		for _, key := range c.ring.Keys() {
			keys = append(keys, key.AEAD)
		}
		return keys, nil
	case payloadEnvelopeVersion:
		if envelope.KeyID == "" {
			return nil, fmt.Errorf("%w: missing key id", ErrInvalidPayloadEnvelope)
		}
		key, ok := c.ring.Lookup(envelope.KeyID)
		if !ok {
			// Key ids are public fingerprints, safe to name in last_error.
			return nil, fmt.Errorf("%w: no configured key has id %q", ErrPayloadDecryption, envelope.KeyID)
		}
		return []cipher.AEAD{key.AEAD}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidPayloadEnvelope, envelope.Version)
	}
}

// decodeEnvelope decodes and size-checks the nonce and ciphertext. Every key
// is AES-256-GCM, so aead supplies the shared nonce size and overhead.
func decodeEnvelope(envelope payloadEnvelope, aead cipher.AEAD) (nonce, data []byte, err error) {
	nonce, err = decodePayloadBase64(envelope.Nonce)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid nonce encoding", ErrInvalidPayloadEnvelope)
	}
	if len(nonce) != aead.NonceSize() {
		return nil, nil, fmt.Errorf("%w: invalid nonce length %d", ErrInvalidPayloadEnvelope, len(nonce))
	}

	data, err = decodePayloadBase64(envelope.Data)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: invalid data encoding", ErrInvalidPayloadEnvelope)
	}
	if len(data) < aead.Overhead() {
		return nil, nil, fmt.Errorf("%w: ciphertext is truncated", ErrInvalidPayloadEnvelope)
	}
	return nonce, data, nil
}

// payloadLooksEncrypted reports whether payload is a JSON object carrying any
// envelope field. Keys are matched case-insensitively, mirroring encoding/json
// struct matching, so nonce-only and mixed-case envelopes are not mistaken for
// legacy plaintext.
func payloadLooksEncrypted(payload json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return false
	}
	for key := range fields {
		switch strings.ToLower(key) {
		case "v", "kid", "nonce", "data":
			return true
		}
	}
	return false
}

// decodePayloadBase64 accepts base64url with or without padding so envelopes
// produced by other encoders remain readable.
func decodePayloadBase64(value string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}
