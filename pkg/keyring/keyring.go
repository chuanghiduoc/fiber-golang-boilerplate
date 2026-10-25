// Package keyring derives AES-256-GCM keys from configured secrets and
// identifies each one by a short key id, so ciphertext envelopes can record
// which key sealed them and a rotation can keep previous keys for decryption.
//
// The key derivation (HKDF-SHA256 with a caller-chosen info string) is the one
// the job payload and webhook secret codecs used before key ids existed, so a
// secret yields the same AES key as before and old envelopes stay readable.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const (
	// keySize is the AES-256 key size in bytes.
	keySize = 32
	// idLength is the number of hex characters in a key id.
	idLength = 8
	// idDomain separates the key-id fingerprint from the encryption key.
	idDomain = "/key-id"
)

// ErrSecretRequired is returned when the active secret is empty.
var ErrSecretRequired = errors.New("keyring: active secret is required")

// Key is one derived AEAD and its id.
type Key struct {
	ID   string
	AEAD cipher.AEAD
}

// Ring is an active key (seals and opens) plus previous keys (open only).
type Ring struct {
	keys []Key // active first
	byID map[string]Key
}

// New derives the active key and every previous key with HKDF-SHA256 under
// info. Empty or duplicate previous secrets are ignored. Two distinct secrets
// whose ids collide are rejected: an envelope's id must name one key.
func New(info, active string, previous ...string) (*Ring, error) {
	if strings.TrimSpace(active) == "" {
		return nil, ErrSecretRequired
	}
	r := &Ring{byID: make(map[string]Key, 1+len(previous))}
	seen := make(map[string]struct{}, 1+len(previous))
	for i, secret := range append([]string{active}, previous...) {
		if _, dup := seen[secret]; dup || (i > 0 && strings.TrimSpace(secret) == "") {
			continue
		}
		seen[secret] = struct{}{}
		key, err := derive(info, secret)
		if err != nil {
			return nil, err
		}
		if _, collision := r.byID[key.ID]; collision {
			return nil, fmt.Errorf("keyring: key id %s collides between two configured secrets; replace one of them", key.ID)
		}
		r.keys = append(r.keys, key)
		r.byID[key.ID] = key
	}
	return r, nil
}

// Active returns the key that seals new envelopes.
func (r *Ring) Active() Key { return r.keys[0] }

// Lookup returns the key with id.
func (r *Ring) Lookup(id string) (Key, bool) {
	key, ok := r.byID[id]
	return key, ok
}

// Keys returns every key, active first. Envelopes written before key ids
// existed are opened by trying each in turn.
func (r *Ring) Keys() []Key { return r.keys }

// ID returns the key id for secret under info: the first 8 hex characters of
// HMAC-SHA256(secret, info+"/key-id"). It is a one-way fingerprint distinct
// from the encryption key.
func ID(info, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(info + idDomain))
	return hex.EncodeToString(mac.Sum(nil))[:idLength]
}

func derive(info, secret string) (Key, error) {
	raw := make([]byte, keySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(secret), nil, []byte(info)), raw); err != nil {
		return Key{}, fmt.Errorf("keyring: derive key: %w", err)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, fmt.Errorf("keyring: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, fmt.Errorf("keyring: create GCM: %w", err)
	}
	return Key{ID: ID(info, secret), AEAD: aead}, nil
}
