// Package apikey implements API-key credentials: generation, hashing at rest,
// verification, rotation grace, and HTTP authentication middleware.
//
// A credential is `<prefix>_<32-byte base64url secret>`. Only the SHA-256 hash
// of the complete credential is stored; the plaintext is returned exactly once
// when a key is created or rotated.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// DefaultPrefix is the display prefix used by generated credentials.
	DefaultPrefix = "fgb"
	// SecretBytes is the amount of entropy in the random portion of a key.
	SecretBytes = 32
	// KeyHashBytes is the size of the SHA-256 digest stored at rest.
	KeyHashBytes = sha256.Size
)

// ErrMalformedKey is returned when a credential does not match the published
// format. Callers must treat it as an authentication failure.
var ErrMalformedKey = errors.New("apikey: malformed key")

var prefixPattern = regexp.MustCompile(`^[a-z0-9]{2,16}$`)

// Credentials is a freshly generated key. Hash is the only value that may be
// persisted; Key must be returned to the caller once and then discarded.
type Credentials struct {
	Key    string
	Hash   string
	Prefix string
}

// Generate creates a new credential with DefaultPrefix.
func Generate() (Credentials, error) {
	return GenerateWithPrefix(DefaultPrefix)
}

// GenerateWithPrefix creates a new credential using prefix. Prefixes are
// lowercase alphanumeric identifiers (2-16 characters) that make keys
// recognizable to users and secret scanners.
func GenerateWithPrefix(prefix string) (Credentials, error) {
	if !prefixPattern.MatchString(prefix) {
		return Credentials{}, fmt.Errorf("%w: invalid prefix", ErrMalformedKey)
	}

	raw := make([]byte, SecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Credentials{}, fmt.Errorf("apikey: generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	key := prefix + "_" + secret
	return Credentials{
		Key:    key,
		Hash:   Hash(key),
		Prefix: prefix,
	}, nil
}

// Hash returns the lowercase hex SHA-256 digest of a complete credential.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ParsePrefix validates a credential's format and returns its prefix. It does
// not verify the key; verification must look up the hash in the store.
func ParsePrefix(key string) (string, error) {
	prefix, secret, ok := strings.Cut(key, "_")
	if !ok || !prefixPattern.MatchString(prefix) {
		return "", ErrMalformedKey
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != SecretBytes {
		return "", ErrMalformedKey
	}
	return prefix, nil
}

// Valid reports whether key matches the published credential format.
func Valid(key string) bool {
	_, err := ParsePrefix(key)
	return err == nil
}
