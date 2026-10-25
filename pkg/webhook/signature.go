// Package webhook implements outbound webhook signing, SSRF-safe delivery,
// retry classification, and inbound signature verification.
//
// Signing follows the Standard Webhooks specification
// (https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md):
//
//   - headers: webhook-id (the message id, stable across retries),
//     webhook-timestamp (Unix seconds), webhook-signature;
//   - signed content: "<webhook-id>.<webhook-timestamp>.<raw body>";
//   - signature: base64 (standard, padded) HMAC-SHA256, sent as "v1,<sig>";
//     several space-separated signatures may be present (secret rotation);
//   - secret: "whsec_" + base64 key; the HMAC key is the decoded bytes.
//
// The event type is part of the signed JSON body, not the signed string.
// Verifiers reject timestamps outside a tolerance so a captured request cannot
// be replayed forever, and deduplicate on webhook-id.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// HeaderID carries the stable message (event) id. Receivers deduplicate
	// on it because delivery is at-least-once.
	HeaderID = "webhook-id"
	// HeaderTimestamp carries the signature Unix timestamp in seconds.
	HeaderTimestamp = "webhook-timestamp"
	// HeaderSignature carries one or more space-separated "v1,<base64>"
	// signatures.
	HeaderSignature = "webhook-signature"
	// SignatureVersion is the symmetric HMAC-SHA256 scheme identifier.
	SignatureVersion = "v1"
	// SecretPrefix marks a Standard Webhooks secret; the rest is base64.
	SecretPrefix = "whsec_"
	// DefaultTolerance bounds timestamp skew for verification.
	DefaultTolerance = 5 * time.Minute
	// UserAgent identifies outbound delivery requests.
	UserAgent = "fiber-golang-boilerplate-webhooks/2.0"
	// secretKeyLen is the size of generated keys (the spec recommends 24-64
	// bytes); minSecretKeyLen is the shortest key accepted.
	secretKeyLen    = 32
	minSecretKeyLen = 24
	// maxTimestampSeconds (9999-12-31T23:59:59Z) rejects values that are not
	// plausible Unix seconds, such as millisecond timestamps far in the future
	// or values whose time.Time arithmetic would saturate.
	maxTimestampSeconds = 253402300799
)

var (
	// ErrEmptySecret is returned when a secret is missing.
	ErrEmptySecret = errors.New("webhook: secret is required")
	// ErrInvalidSecret is returned when a secret is not "whsec_" followed by
	// a standard base64 key of at least 24 bytes.
	ErrInvalidSecret = errors.New("webhook: secret is not a Standard Webhooks secret")
	// ErrMissingSignature is returned when the signature header is absent.
	ErrMissingSignature = errors.New("webhook: signature header is missing")
	// ErrInvalidSignature is returned when no signature authenticates the body.
	ErrInvalidSignature = errors.New("webhook: signature is invalid")
	// ErrMissingID is returned when the webhook-id header is absent.
	ErrMissingID = errors.New("webhook: id header is missing")
	// ErrMissingTimestamp is returned when the timestamp header is absent.
	ErrMissingTimestamp = errors.New("webhook: timestamp header is missing")
	// ErrInvalidTimestamp is returned when the timestamp is not Unix seconds.
	ErrInvalidTimestamp = errors.New("webhook: timestamp is invalid")
	// ErrTimestampOutOfRange is returned when the timestamp is outside the
	// configured tolerance. Both stale and far-future timestamps are rejected.
	ErrTimestampOutOfRange = errors.New("webhook: timestamp is outside the allowed tolerance")
)

// NewSecret returns a fresh "whsec_<base64>" secret with a 32-byte key.
func NewSecret() (string, error) {
	key := make([]byte, secretKeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("webhook: generate secret: %w", err)
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(key), nil
}

// DecodeSecret returns the HMAC key of a "whsec_<base64>" secret, exactly as
// the official Standard Webhooks libraries derive it.
func DecodeSecret(secret string) ([]byte, error) {
	if secret == "" {
		return nil, ErrEmptySecret
	}
	encoded, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok {
		return nil, ErrInvalidSecret
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) < minSecretKeyLen {
		return nil, ErrInvalidSecret
	}
	return key, nil
}

// Sign returns the "v1,<base64>" signature of "<id>.<timestamp>.<body>" under
// key (a decoded secret). The timestamp is truncated to Unix seconds so it
// round-trips through the webhook-timestamp header.
func Sign(key []byte, id string, timestamp time.Time, body []byte) string {
	return SignatureVersion + "," + base64.StdEncoding.EncodeToString(digest(key, id, timestamp.Unix(), body))
}

func digest(key []byte, id string, unixSeconds int64, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(strconv.FormatInt(unixSeconds, 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

// SignatureHeader joins signatures (each "v1,<base64>") into the
// space-separated webhook-signature value.
func SignatureHeader(signatures ...string) string {
	return strings.Join(signatures, " ")
}

// Verify checks a webhook-signature header over id, timestamp, and body
// against key. A non-positive tolerance uses DefaultTolerance. Every v1
// candidate is compared in constant time; other versions are ignored, as the
// specification requires.
func Verify(key []byte, id string, timestamp time.Time, header string, body []byte, tolerance time.Duration, now time.Time) error {
	if len(key) == 0 {
		return ErrEmptySecret
	}
	if id == "" {
		return ErrMissingID
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	if timestamp.IsZero() {
		return ErrMissingTimestamp
	}
	// Compare against window bounds instead of computing now.Sub(timestamp):
	// time.Duration saturates for instants ~292 years apart and negating
	// math.MinInt64 overflows back to negative, which would let a far-future
	// or millisecond-unit timestamp pass any tolerance.
	if timestamp.Before(now.Add(-tolerance)) || timestamp.After(now.Add(tolerance)) {
		return fmt.Errorf("%w: tolerance %s", ErrTimestampOutOfRange, tolerance)
	}

	candidates := v1Signatures(header)
	if len(candidates) == 0 {
		return ErrMissingSignature
	}
	expected := digest(key, id, timestamp.Unix(), body)
	for _, candidate := range candidates {
		if hmac.Equal(candidate, expected) {
			return nil
		}
	}
	return ErrInvalidSignature
}

// VerifyRequest verifies the Standard Webhooks headers on an inbound request
// with key (a decoded secret, see DecodeSecret). The id is taken verbatim from
// webhook-id, so callers must use that same raw header value afterwards.
func VerifyRequest(key []byte, headers http.Header, body []byte, tolerance time.Duration, now time.Time) error {
	if len(key) == 0 {
		return ErrEmptySecret
	}
	id := headers.Get(HeaderID)
	if id == "" {
		return ErrMissingID
	}
	rawTimestamp := strings.TrimSpace(headers.Get(HeaderTimestamp))
	if rawTimestamp == "" {
		return ErrMissingTimestamp
	}
	seconds, err := strconv.ParseInt(rawTimestamp, 10, 64)
	if err != nil || seconds <= 0 || seconds > maxTimestampSeconds {
		return ErrInvalidTimestamp
	}
	signature := headers.Get(HeaderSignature)
	if strings.TrimSpace(signature) == "" {
		return ErrMissingSignature
	}
	return Verify(key, id, time.Unix(seconds, 0).UTC(), signature, body, tolerance, now)
}

// v1Signatures extracts every decodable v1 digest from a webhook-signature
// header. Unknown versions (such as the asymmetric v1a) and malformed entries
// are ignored; a header with no v1 digest yields no candidates.
func v1Signatures(header string) [][]byte {
	fields := strings.Fields(header)
	out := make([][]byte, 0, len(fields))
	for _, field := range fields {
		version, encoded, ok := strings.Cut(field, ",")
		if !ok || version != SignatureVersion {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(sig) != sha256.Size {
			continue
		}
		out = append(out, sig)
	}
	return out
}
