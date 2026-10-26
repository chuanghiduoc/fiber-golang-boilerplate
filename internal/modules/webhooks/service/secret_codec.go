package service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/keyring"
)

const (
	// secretKeyInfo is the HKDF info; changing it would make every stored
	// secret unreadable.
	secretKeyInfo = "fiber-golang-boilerplate/webhooks/secret/v1"
	// secretEnvelopeVer is the only envelope version Open accepts. It names
	// the sealing key and binds the purpose, tenant, and owning entity into
	// the AAD.
	secretEnvelopeVer = 3
)

// SecretPurpose domain-separates endpoint secrets from inbound-source secrets.
type SecretPurpose string

const (
	// PurposeEndpointSecret seals outbound endpoint signing secrets and their
	// delivery snapshots.
	PurposeEndpointSecret SecretPurpose = "endpoint"
	// PurposeInboundSourceSecret seals inbound receiver verification secrets.
	PurposeInboundSourceSecret SecretPurpose = "inbound_source"
)

// SecretBinding is the authenticated associated data of a sealed webhook
// secret. It prevents a ciphertext from being swapped to another endpoint,
// source, or tenant by someone with database write access.
type SecretBinding struct {
	Purpose  SecretPurpose
	TenantID int64
	EntityID int64
}

// EndpointSecretBinding binds a secret to one tenant-owned endpoint. Delivery
// snapshots are copies of the endpoint secret, so they use the same binding.
func EndpointSecretBinding(tenantID, endpointID int64) SecretBinding {
	return SecretBinding{Purpose: PurposeEndpointSecret, TenantID: tenantID, EntityID: endpointID}
}

// InboundSourceSecretBinding binds a secret to one tenant-owned inbound source.
func InboundSourceSecretBinding(tenantID, sourceID int64) SecretBinding {
	return SecretBinding{Purpose: PurposeInboundSourceSecret, TenantID: tenantID, EntityID: sourceID}
}

func (b SecretBinding) valid() bool {
	return b.Purpose != "" && b.TenantID > 0 && b.EntityID > 0
}

var (
	// ErrSecretKeyRequired is returned when the codec is created without a key.
	ErrSecretKeyRequired = errors.New("webhooks: secret encryption key is required")
	// ErrNilSecretCodec is returned by Seal/Open on a nil codec.
	ErrNilSecretCodec = errors.New("webhooks: secret codec is nil")
	// ErrInvalidSecretEnvelope is returned for malformed or unsupported
	// ciphertext envelopes.
	ErrInvalidSecretEnvelope = errors.New("webhooks: invalid secret envelope")
	// ErrInvalidSecretBinding is returned when a new envelope is sealed without
	// a complete purpose/tenant/entity binding.
	ErrInvalidSecretBinding = errors.New("webhooks: invalid secret binding")
	// ErrSecretDecryption is returned when the envelope cannot be
	// authenticated with any configured key, including an envelope whose key
	// id is not configured or whose binding does not match.
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

// secretAAD binds the envelope version, key id, and the secret's purpose,
// tenant, and owning entity to the ciphertext.
func secretAAD(keyID string, binding SecretBinding) []byte {
	return []byte(fmt.Sprintf("%s|v%d|%s|%s|%d|%d",
		secretKeyInfo, secretEnvelopeVer, keyID, binding.Purpose, binding.TenantID, binding.EntityID))
}

// Seal encrypts plaintext with the active key and returns a versioned JSON
// envelope naming the key and authenticating binding.
func (c *SecretCodec) Seal(binding SecretBinding, plaintext []byte) (string, error) {
	if c == nil || c.ring == nil {
		return "", ErrNilSecretCodec
	}
	if !binding.valid() {
		return "", ErrInvalidSecretBinding
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
		Data:    base64.RawURLEncoding.EncodeToString(key.AEAD.Seal(nil, nonce, plaintext, secretAAD(key.ID, binding))),
	})
	if err != nil {
		return "", fmt.Errorf("webhooks: encode secret envelope: %w", err)
	}
	return string(sealed), nil
}

// Open decrypts a version 3 secret envelope. The binding it was sealed with is
// required in the AAD, so a ciphertext copied to another purpose, tenant, or
// row fails authentication. Older unbound envelope versions are rejected
// outright; no database has data from before v3.
func (c *SecretCodec) Open(binding SecretBinding, ciphertext string) ([]byte, error) {
	if c == nil || c.ring == nil {
		return nil, ErrNilSecretCodec
	}
	var envelope secretEnvelope
	if err := json.Unmarshal([]byte(ciphertext), &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSecretEnvelope, err)
	}
	if envelope.Version != secretEnvelopeVer {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidSecretEnvelope, envelope.Version)
	}
	if !binding.valid() {
		return nil, fmt.Errorf("%w: missing binding", ErrInvalidSecretEnvelope)
	}
	if envelope.KeyID == "" {
		return nil, fmt.Errorf("%w: missing key id", ErrInvalidSecretEnvelope)
	}
	key, ok := c.ring.Lookup(envelope.KeyID)
	if !ok {
		return nil, fmt.Errorf("%w: no configured key has id %q", ErrSecretDecryption, envelope.KeyID)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != key.AEAD.NonceSize() {
		return nil, fmt.Errorf("%w: invalid nonce", ErrInvalidSecretEnvelope)
	}
	data, err := base64.RawURLEncoding.DecodeString(envelope.Data)
	if err != nil || len(data) < key.AEAD.Overhead() {
		return nil, fmt.Errorf("%w: invalid data", ErrInvalidSecretEnvelope)
	}
	plaintext, err := key.AEAD.Open(nil, nonce, data, secretAAD(envelope.KeyID, binding))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSecretDecryption, err)
	}
	return plaintext, nil
}
