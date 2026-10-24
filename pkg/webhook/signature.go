// Package webhook implements outbound webhook signing, SSRF-safe delivery,
// retry classification, and inbound signature verification.
//
// Signatures use HMAC-SHA256 over "<unix-timestamp>.<raw-body>" and are sent as
// "v1=<hex>" in X-Webhook-Signature. Verifiers must reject timestamps outside a
// configured tolerance so a captured request cannot be replayed forever.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// HeaderID carries the stable event ID.
	HeaderID = "X-Webhook-Id"
	// HeaderDelivery carries the delivery attempt identity. Receivers should
	// deduplicate on this value because delivery is at-least-once.
	HeaderDelivery = "X-Webhook-Delivery"
	// HeaderEvent carries the event type.
	HeaderEvent = "X-Webhook-Event"
	// HeaderTimestamp carries the signature Unix timestamp in seconds.
	HeaderTimestamp = "X-Webhook-Timestamp"
	// HeaderSignature carries one or more "v1=<hex>" signatures.
	HeaderSignature = "X-Webhook-Signature"
	// HeaderAttempt carries the 1-based delivery attempt number.
	HeaderAttempt = "X-Webhook-Attempt"
	// SignaturePrefix versions the HMAC scheme.
	SignaturePrefix = "v1="
	// DefaultTolerance bounds timestamp skew for verification.
	DefaultTolerance = 5 * time.Minute
	// UserAgent identifies outbound delivery requests.
	UserAgent = "fiber-golang-boilerplate-webhooks/1.0"
)

var (
	// ErrEmptySecret is returned when a secret is missing.
	ErrEmptySecret = errors.New("webhook: secret is required")
	// ErrMissingSignature is returned when the signature header is absent.
	ErrMissingSignature = errors.New("webhook: signature header is missing")
	// ErrInvalidSignature is returned when no signature authenticates the body.
	ErrInvalidSignature = errors.New("webhook: signature is invalid")
	// ErrMissingTimestamp is returned when the timestamp header is absent.
	ErrMissingTimestamp = errors.New("webhook: timestamp header is missing")
	// ErrInvalidTimestamp is returned when the timestamp is not Unix seconds.
	ErrInvalidTimestamp = errors.New("webhook: timestamp is invalid")
	// ErrTimestampOutOfRange is returned when the timestamp is outside the
	// configured tolerance. Both stale and far-future timestamps are rejected.
	ErrTimestampOutOfRange = errors.New("webhook: timestamp is outside the allowed tolerance")
)

// Sign returns the lowercase hex HMAC-SHA256 of "<timestamp>.<body>". The
// timestamp is truncated to Unix seconds so it round-trips through the
// X-Webhook-Timestamp header.
func Sign(secret []byte, timestamp time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp.Unix(), 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader returns the wire value for a signature digest.
func SignatureHeader(signature string) string {
	return SignaturePrefix + signature
}

// Verify checks a signature over body at timestamp against secret. A
// non-positive tolerance uses DefaultTolerance. Multiple comma- or
// whitespace-separated signatures are accepted so a sender can publish several
// valid digests during a key transition; every candidate is compared in
// constant time.
func Verify(secret []byte, timestamp time.Time, signature string, body []byte, tolerance time.Duration, now time.Time) error {
	if len(secret) == 0 {
		return ErrEmptySecret
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	if timestamp.IsZero() {
		return ErrMissingTimestamp
	}
	skew := now.Sub(timestamp)
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return fmt.Errorf("%w: skew %s exceeds %s", ErrTimestampOutOfRange, skew.Truncate(time.Second), tolerance)
	}

	candidates := splitSignatures(signature)
	if len(candidates) == 0 {
		return ErrMissingSignature
	}
	expected := Sign(secret, timestamp, body)
	for _, candidate := range candidates {
		if hmac.Equal([]byte(candidate), []byte(expected)) {
			return nil
		}
	}
	return ErrInvalidSignature
}

// VerifyRequest verifies the standard headers on an inbound request.
func VerifyRequest(secret []byte, headers http.Header, body []byte, tolerance time.Duration, now time.Time) error {
	if len(secret) == 0 {
		return ErrEmptySecret
	}
	rawTimestamp := strings.TrimSpace(headers.Get(HeaderTimestamp))
	if rawTimestamp == "" {
		return ErrMissingTimestamp
	}
	seconds, err := strconv.ParseInt(rawTimestamp, 10, 64)
	if err != nil {
		return ErrInvalidTimestamp
	}
	signature := headers.Get(HeaderSignature)
	if strings.TrimSpace(signature) == "" {
		return ErrMissingSignature
	}
	return Verify(secret, time.Unix(seconds, 0).UTC(), signature, body, tolerance, now)
}

// splitSignatures extracts every v1 digest from a signature header. Unknown
// schemes are ignored; a header with no v1 digest yields no candidates and is
// therefore invalid.
func splitSignatures(header string) []string {
	fields := strings.FieldsFunc(header, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		digest, ok := strings.CutPrefix(field, SignaturePrefix)
		if !ok {
			continue
		}
		digest = strings.TrimSpace(digest)
		if digest == "" {
			continue
		}
		if _, err := hex.DecodeString(digest); err != nil {
			continue
		}
		if len(digest) != sha256.Size*2 {
			continue
		}
		out = append(out, strings.ToLower(digest))
	}
	return out
}
