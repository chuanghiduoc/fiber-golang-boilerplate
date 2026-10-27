// Package banhash computes the keyed pseudonymous marker a tenant ban keeps
// after the banned account is removed or erased.
//
// The marker is HMAC-SHA256 over the normalized address under a key derived
// from the data-encryption key set with HKDF-SHA256. It is deliberately not an
// unkeyed hash: a dump or backup of memberships must not let a reader confirm
// or dictionary-recover a banned address. The key id stored beside the marker
// records which configured key wrote it, so lookups can try the active key and
// every previous key during a rotation.
//
// Removing a key from the configured set makes bans written with it
// ineffective: after erasure there is no address left to re-hash, so the
// marker must be matched with the key that wrote it.
package banhash

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/keyring"
)

const (
	// Info domain-separates the ban-marker key from every other use of the
	// data-encryption key set. It is also the HKDF info, so changing it
	// invalidates every stored marker.
	Info = "fgb/ban-email/v1"
	// keySize is the HMAC-SHA256 key length in bytes.
	keySize = 32
)

// ErrKeyRequired is returned when no active data-encryption key is configured.
var ErrKeyRequired = errors.New("banhash: active key is required")

type key struct {
	id     string
	macKey []byte
}

// Hasher computes ban markers with an active key and matches them with every
// configured key (active first).
type Hasher struct {
	keys []key
}

// New derives the active marker key and the previous keys with HKDF-SHA256
// under Info. Empty and duplicate previous keys are ignored, and two distinct
// keys whose ids collide are rejected: a stored id must name exactly one key.
func New(active string, previous ...string) (*Hasher, error) {
	if strings.TrimSpace(active) == "" {
		return nil, ErrKeyRequired
	}
	h := &Hasher{}
	seenSecrets := make(map[string]struct{}, 1+len(previous))
	seenIDs := make(map[string]struct{}, 1+len(previous))
	for i, secret := range append([]string{active}, previous...) {
		if _, dup := seenSecrets[secret]; dup || (i > 0 && strings.TrimSpace(secret) == "") {
			continue
		}
		seenSecrets[secret] = struct{}{}
		macKey, err := deriveKey(secret)
		if err != nil {
			return nil, err
		}
		id := keyring.ID(Info, secret)
		if _, collision := seenIDs[id]; collision {
			return nil, fmt.Errorf("banhash: key id %s collides between two configured keys; replace one of them", id)
		}
		seenIDs[id] = struct{}{}
		h.keys = append(h.keys, key{id: id, macKey: macKey})
	}
	return h, nil
}

// Marker returns the marker and key id a new ban records.
func (h *Hasher) Marker(email string) (hash []byte, kid string) {
	active := h.keys[0]
	return h.mac(active, email), active.id
}

// Markers returns the marker under every configured key, active first. A
// lookup matches against all of them, so a ban written before a rotation keeps
// working while its key remains configured.
func (h *Hasher) Markers(email string) [][]byte {
	hashes := make([][]byte, len(h.keys))
	for i, key := range h.keys {
		hashes[i] = h.mac(key, email)
	}
	return hashes
}

// KeyIDs returns the configured key ids, active first.
func (h *Hasher) KeyIDs() []string {
	ids := make([]string, len(h.keys))
	for i, key := range h.keys {
		ids[i] = key.id
	}
	return ids
}

// mac computes the raw HMAC-SHA256 digest of the normalized address.
// Normalization must stay identical to identity NormalizeEmail: a marker is
// only found when both sides canonicalize the address the same way.
func (h *Hasher) mac(key key, email string) []byte {
	mac := hmac.New(sha256.New, key.macKey)
	mac.Write([]byte(normalizeEmail(email)))
	return mac.Sum(nil)
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func deriveKey(secret string) ([]byte, error) {
	raw := make([]byte, keySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(secret), nil, []byte(Info)), raw); err != nil {
		return nil, fmt.Errorf("banhash: derive key: %w", err)
	}
	return raw, nil
}
