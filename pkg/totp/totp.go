// Package totp implements the RFC 6238 time-based one-time password used as
// the second authentication factor: SHA-1, 6 digits, 30-second steps.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP requires HMAC-SHA1 for authenticator compatibility
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Digits is the code length; RFC 6238 defaults to 6.
	Digits = 6
	// Period is the time step.
	Period = 30 * time.Second
	// SecretBytes is the 160-bit secret RFC 4226 recommends for HMAC-SHA1.
	SecretBytes = 20
	// Window is how many adjacent steps a code is accepted in either
	// direction, absorbing small clock skew.
	Window = 1
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a new random base32 secret (20 bytes, no padding).
func GenerateSecret() (string, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("totp: generate secret: %w", err)
	}
	return encoding.EncodeToString(b), nil
}

// Step returns the RFC 6238 time step of t.
func Step(t time.Time) int64 {
	return t.Unix() / int64(Period/time.Second)
}

// Code returns the code for the step containing t.
func Code(secret string, t time.Time) (string, error) {
	return CodeAt(secret, Step(t))
}

// CodeAt returns the code for one step.
func CodeAt(secret string, step int64) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // step is a non-negative RFC 6238 counter
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, value%1_000_000), nil
}

// Validate checks code against at and the Window adjacent steps, returning
// the matched step. Replay protection (a step may not be used twice) is the
// caller's atomic database guard.
func Validate(secret, code string, at time.Time) (step int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != Digits {
		return 0, false
	}
	if _, err := strconv.Atoi(code); err != nil {
		return 0, false
	}
	now := Step(at)
	for delta := int64(-Window); delta <= Window; delta++ {
		candidate, err := CodeAt(secret, now+delta)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return now + delta, true
		}
	}
	return 0, false
}

// URI builds the otpauth:// provisioning URI an authenticator app scans.
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(Digits))
	q.Set("period", strconv.Itoa(int(Period/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func decodeSecret(secret string) ([]byte, error) {
	secret = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	key, err := encoding.DecodeString(secret)
	if err != nil {
		return nil, fmt.Errorf("totp: invalid secret: %w", err)
	}
	return key, nil
}
