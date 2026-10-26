// Package apikey implements API-key credentials: generation, hashing at rest,
// verification, rotation grace, and HTTP authentication middleware.
//
// A credential is `<prefix>_<43 base62 chars of 32 random bytes><6 base62 chars
// of the CRC32 checksum of the random part>`. The checksum lets a verifier
// reject a mistyped or invented key before any database lookup, following
// GitHub's token format and secret-scanning practice. Only the SHA-256 hash of
// the complete credential is stored; the plaintext is returned exactly once
// when a key is created or rotated.
//
// Keys issued before the checksum existed are `<prefix>_<32-byte base64url
// secret>`. They remain valid for verification but are deprecated: rotation
// issues the current format.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
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

	// randomPartLen is the fixed-length base62 encoding of SecretBytes. A
	// fixed width keeps the new format unambiguous with the legacy one.
	randomPartLen = 43
	// checksumLen is the fixed-width base62 CRC32.
	checksumLen = 6
	// newSecretLen is the total suffix length of a current-format key.
	newSecretLen = randomPartLen + checksumLen
	// legacySecretLen is the fixed base64url length of SecretBytes.
	legacySecretLen = 43
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
	randomPart := encodeBase62(raw)
	secret := randomPart + checksumBase62(randomPart)
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
// not verify the key against the store, but it does verify the current-format
// CRC32 checksum, so a corrupted key is rejected before any lookup. Legacy
// base64url credentials are accepted for backward compatibility.
func ParsePrefix(key string) (string, error) {
	prefix, secret, ok := strings.Cut(key, "_")
	if !ok || !prefixPattern.MatchString(prefix) {
		return "", ErrMalformedKey
	}
	switch len(secret) {
	case newSecretLen:
		if err := validateChecksum(secret); err != nil {
			return "", err
		}
	case legacySecretLen:
		decoded, err := base64.RawURLEncoding.DecodeString(secret)
		if err != nil || len(decoded) != SecretBytes {
			return "", ErrMalformedKey
		}
	default:
		return "", ErrMalformedKey
	}
	return prefix, nil
}

// Valid reports whether key matches a published credential format, including
// the current-format checksum.
func Valid(key string) bool {
	_, err := ParsePrefix(key)
	return err == nil
}

// validateChecksum checks a current-format suffix: a canonical 43-character
// base62 random part followed by its 6-character CRC32 checksum.
func validateChecksum(secret string) error {
	randomPart := secret[:randomPartLen]
	raw, err := decodeBase62(randomPart)
	if err != nil || encodeBase62(raw) != randomPart {
		return ErrMalformedKey
	}
	expected := checksumBase62(randomPart)
	if subtle.ConstantTimeCompare([]byte(secret[randomPartLen:]), []byte(expected)) != 1 {
		return ErrMalformedKey
	}
	return nil
}

// encodeBase62 renders raw as a fixed-width (randomPartLen) base62 number.
func encodeBase62(raw []byte) string {
	encoded := new(big.Int).SetBytes(raw).Text(62)
	if len(encoded) > randomPartLen {
		// Unreachable for SecretBytes; guard the fixed-width invariant.
		panic("apikey: base62 random part exceeds its fixed width")
	}
	return strings.Repeat("0", randomPartLen-len(encoded)) + encoded
}

// decodeBase62 parses a base62 number and returns its big-endian bytes. The
// caller must re-encode to reject non-canonical (non-zero-padded) input.
func decodeBase62(value string) ([]byte, error) {
	number, ok := new(big.Int).SetString(value, 62)
	if !ok || number.Sign() < 0 {
		return nil, ErrMalformedKey
	}
	raw := number.Bytes()
	if len(raw) > SecretBytes {
		return nil, ErrMalformedKey
	}
	padded := make([]byte, SecretBytes)
	copy(padded[SecretBytes-len(raw):], raw)
	return padded, nil
}

// checksumBase62 renders the CRC32 of randomPart as exactly checksumLen
// base62 digits.
func checksumBase62(randomPart string) string {
	sum := crc32.ChecksumIEEE([]byte(randomPart))
	encoded := new(big.Int).SetUint64(uint64(sum)).Text(62)
	return strings.Repeat("0", checksumLen-len(encoded)) + encoded
}
