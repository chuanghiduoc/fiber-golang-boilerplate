package service

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
	secretKeyInfo      = "fiber-golang-boilerplate/webhooks/secret/v1"
	secretEnvelopeVer  = 1
	secretKeySize      = 32
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
	// ErrSecretDecryption is returned when the envelope cannot be authenticated.
	ErrSecretDecryption = errors.New("webhooks: secret decryption failed")
)

// SecretCodec seals endpoint and inbound-source signing secrets with
// AES-256-GCM. The key is derived from the configured application secret with
// HKDF-SHA256 and domain separation.
type SecretCodec struct {
	aead cipher.AEAD
}

type secretEnvelope struct {
	Version int    `json:"v"`
	Nonce   string `json:"nonce"`
	Data    string `json:"data"`
}

// NewSecretCodec derives an AES-256 key from secret.
func NewSecretCodec(secret string) (*SecretCodec, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrSecretKeyRequired
	}
	key := make([]byte, secretKeySize)
	reader := hkdf.New(sha256.New, []byte(secret), nil, []byte(secretKeyInfo))
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("webhooks: derive secret key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("webhooks: create secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webhooks: create secret GCM: %w", err)
	}
	return &SecretCodec{aead: aead}, nil
}

// Seal encrypts plaintext and returns a versioned JSON envelope.
func (c *SecretCodec) Seal(plaintext []byte) (string, error) {
	if c == nil || c.aead == nil {
		return "", ErrNilSecretCodec
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("webhooks: generate secret nonce: %w", err)
	}
	sealed, err := json.Marshal(secretEnvelope{
		Version: secretEnvelopeVer,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(c.aead.Seal(nil, nonce, plaintext, []byte(secretKeyInfo))),
	})
	if err != nil {
		return "", fmt.Errorf("webhooks: encode secret envelope: %w", err)
	}
	return string(sealed), nil
}

// Open decrypts a secret envelope.
func (c *SecretCodec) Open(ciphertext string) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, ErrNilSecretCodec
	}
	var envelope secretEnvelope
	if err := json.Unmarshal([]byte(ciphertext), &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSecretEnvelope, err)
	}
	if envelope.Version != secretEnvelopeVer {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidSecretEnvelope, envelope.Version)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != c.aead.NonceSize() {
		return nil, fmt.Errorf("%w: invalid nonce", ErrInvalidSecretEnvelope)
	}
	data, err := base64.RawURLEncoding.DecodeString(envelope.Data)
	if err != nil || len(data) < c.aead.Overhead() {
		return nil, fmt.Errorf("%w: invalid data", ErrInvalidSecretEnvelope)
	}
	plaintext, err := c.aead.Open(nil, nonce, data, []byte(secretKeyInfo))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSecretDecryption, err)
	}
	return plaintext, nil
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
