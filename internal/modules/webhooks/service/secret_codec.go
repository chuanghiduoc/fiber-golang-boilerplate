package service

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/keyring"
)

const (
	// secretKeyInfo is the HKDF info and the legacy AAD; changing it would
	// make every stored secret unreadable.
	secretKeyInfo = "fiber-golang-boilerplate/webhooks/secret/v1"
	// legacySecretEnvelopeVer envelopes carry no key id; Open tries every key.
	legacySecretEnvelopeVer = 1
	// secretEnvelopeVer is written by Seal: it names the sealing key and
	// binds the id into the AAD.
	secretEnvelopeVer  = 2
	generatedSecretLen = 32
)

var (
	// ErrSecretKeyRequired is returned when the codec is created without a key.
	ErrSecretKeyRequired = errors.New("webhooks: secret encryption key is required")
	// ErrNilSecretCodec is returned by Seal/Open on a nil codec.
	ErrNilSecretCodec = errors.New("webhooks: secret codec is nil")
	// ErrInvalidSecretEnvelope is returned for malformed or unsupported
	// ciphertext envelopes.
	ErrInvalidSecretEnvelope = errors.New("webhooks: invalid secret envelope")
	// ErrSecretDecryption is returned when the envelope cannot be
	// authenticated with any configured key, including an envelope whose key
	// id is not configured.
	ErrSecretDecryption = errors.New("webhooks: secret decryption failed")
)

// SecretCodec seals endpoint and inbound-source signing secrets with
// AES-256-GCM. Keys are derived from the configured secrets with HKDF-SHA256
// and domain separation. Seal uses the active key; Open also accepts the
// previous keys (WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS), so the encryption
// key can rotate without making stored secrets unreadable. Rows keep the key
// that sealed them until the secret itself is rotated.
type SecretCodec struct {
	ring *keyring.Ring
}

type secretEnvelope struct {
	Version int    `json:"v"`
	KeyID   string `json:"kid,omitempty"`
	Nonce   string `json:"nonce"`
	Data    string `json:"data"`
}

// NewSecretCodec derives the active key from secret and decryption-only keys
// from previous.
func NewSecretCodec(secret string, previous ...string) (*SecretCodec, error) {
	ring, err := keyring.New(secretKeyInfo, secret, previous...)
	if errors.Is(err, keyring.ErrSecretRequired) {
		return nil, ErrSecretKeyRequired
	}
	if err != nil {
		return nil, fmt.Errorf("webhooks: secret keys: %w", err)
	}
	return &SecretCodec{ring: ring}, nil
}

// secretAAD binds the envelope version and key id to the ciphertext. Legacy
// envelopes used the bare domain string.
func secretAAD(version int, keyID string) []byte {
	if version == legacySecretEnvelopeVer {
		return []byte(secretKeyInfo)
	}
	return []byte(fmt.Sprintf("%s|v%d|%s", secretKeyInfo, version, keyID))
}

// Seal encrypts plaintext with the active key and returns a versioned JSON
// envelope naming the key.
func (c *SecretCodec) Seal(plaintext []byte) (string, error) {
	if c == nil || c.ring == nil {
		return "", ErrNilSecretCodec
	}
	key := c.ring.Active()
	nonce := make([]byte, key.AEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("webhooks: generate secret nonce: %w", err)
	}
	sealed, err := json.Marshal(secretEnvelope{
		Version: secretEnvelopeVer,
		KeyID:   key.ID,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(key.AEAD.Seal(nil, nonce, plaintext, secretAAD(secretEnvelopeVer, key.ID))),
	})
	if err != nil {
		return "", fmt.Errorf("webhooks: encode secret envelope: %w", err)
	}
	return string(sealed), nil
}

// Open decrypts a secret envelope: version 2 with the key its id names,
// version 1 (no id) by trying every configured key.
func (c *SecretCodec) Open(ciphertext string) ([]byte, error) {
	if c == nil || c.ring == nil {
		return nil, ErrNilSecretCodec
	}
	var envelope secretEnvelope
	if err := json.Unmarshal([]byte(ciphertext), &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSecretEnvelope, err)
	}
	keys, err := c.envelopeKeys(envelope)
	if err != nil {
		return nil, err
	}
	aead := c.ring.Active().AEAD
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: invalid nonce", ErrInvalidSecretEnvelope)
	}
	data, err := base64.RawURLEncoding.DecodeString(envelope.Data)
	if err != nil || len(data) < aead.Overhead() {
		return nil, fmt.Errorf("%w: invalid data", ErrInvalidSecretEnvelope)
	}
	aad := secretAAD(envelope.Version, envelope.KeyID)
	var openErr error
	for _, key := range keys {
		plaintext, err := key.Open(nil, nonce, data, aad)
		if err == nil {
			return plaintext, nil
		}
		openErr = err
	}
	return nil, fmt.Errorf("%w: %v", ErrSecretDecryption, openErr)
}

// envelopeKeys returns the keys that may open envelope.
func (c *SecretCodec) envelopeKeys(envelope secretEnvelope) ([]cipher.AEAD, error) {
	switch envelope.Version {
	case legacySecretEnvelopeVer:
		keys := make([]cipher.AEAD, 0, len(c.ring.Keys()))
		for _, key := range c.ring.Keys() {
			keys = append(keys, key.AEAD)
		}
		return keys, nil
	case secretEnvelopeVer:
		if envelope.KeyID == "" {
			return nil, fmt.Errorf("%w: missing key id", ErrInvalidSecretEnvelope)
		}
		key, ok := c.ring.Lookup(envelope.KeyID)
		if !ok {
			return nil, fmt.Errorf("%w: no configured key has id %q", ErrSecretDecryption, envelope.KeyID)
		}
		return []cipher.AEAD{key.AEAD}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidSecretEnvelope, envelope.Version)
	}
}

// GenerateSecret creates a URL-safe random signing secret. The prefix makes
// the secret recognisable in configuration and receiver logs.
func GenerateSecret(prefix string) (string, error) {
	raw := make([]byte, generatedSecretLen)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("webhooks: generate secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
