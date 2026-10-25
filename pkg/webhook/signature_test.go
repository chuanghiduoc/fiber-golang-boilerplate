package webhook

import (
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testID = "msg_1"

// testKey is a fixed 32-byte HMAC key ("whsec_" + base64 of it is the secret).
var testKey = []byte("0123456789abcdef0123456789abcdef")

// TestSign_StandardWebhooksVector checks the known-answer example published
// with the Standard Webhooks reference libraries, so this implementation
// interoperates with them byte for byte.
func TestSign_StandardWebhooksVector(t *testing.T) {
	key, err := DecodeSecret(SecretPrefix + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw") // public spec example key
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	got := Sign(key, "msg_p5jXN8AQM9LWM0D4loKWxJek", time.Unix(1614265330, 0), []byte(`{"test": 2432232314}`))
	const want = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	if got != want {
		t.Fatalf("Sign() = %q, want %q", got, want)
	}
}

func TestSecrets(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatalf("secret %q lacks %s", secret, SecretPrefix)
	}
	key, err := DecodeSecret(secret)
	if err != nil || len(key) != secretKeyLen {
		t.Fatalf("DecodeSecret(NewSecret()) = %d bytes, %v", len(key), err)
	}
	other, _ := NewSecret()
	if other == secret {
		t.Fatal("NewSecret returned the same secret twice")
	}

	for name, bad := range map[string]string{
		"no prefix":         base64.StdEncoding.EncodeToString(testKey),
		"legacy inbound":    "whin_" + base64.StdEncoding.EncodeToString(testKey),
		"legacy base64url":  "whsec_" + base64.RawURLEncoding.EncodeToString(append([]byte{0xfb, 0xff}, testKey...)),
		"too short":         "whsec_" + base64.StdEncoding.EncodeToString([]byte("short")),
		"not base64 at all": "whsec_***",
	} {
		if _, err := DecodeSecret(bad); !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: DecodeSecret error = %v, want ErrInvalidSecret", name, err)
		}
	}
	if _, err := DecodeSecret(""); !errors.Is(err, ErrEmptySecret) {
		t.Errorf("empty secret error = %v, want ErrEmptySecret", err)
	}
}

func TestSignAndVerify(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"file.uploaded"}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	signature := Sign(testKey, testID, now, body)
	if !strings.HasPrefix(signature, "v1,") {
		t.Fatalf("signature %q must start with v1,", signature)
	}
	if err := Verify(testKey, testID, now, signature, body, time.Minute, now); err != nil {
		t.Fatalf("Verify() rejected a valid signature: %v", err)
	}
	if err := Verify(testKey, testID, now, signature, []byte(`tampered`), time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Verify() tampered body error = %v, want ErrInvalidSignature", err)
	}
	if err := Verify([]byte("wrong-key-wrong-key-wrong-key!!!"), testID, now, signature, body, time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Verify() wrong key error = %v, want ErrInvalidSignature", err)
	}
}

// TestVerify_BindsID proves a captured signature cannot be replayed under a
// fresh id to slip past webhook-id deduplication.
func TestVerify_BindsID(t *testing.T) {
	body := []byte(`{"x":1}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	signature := Sign(testKey, testID, now, body)

	if err := Verify(testKey, "msg_2", now, signature, body, time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("replay with a new id error = %v, want ErrInvalidSignature", err)
	}
	if err := Verify(testKey, "", now, signature, body, time.Minute, now); !errors.Is(err, ErrMissingID) {
		t.Fatalf("missing id error = %v, want ErrMissingID", err)
	}
}

func TestVerify_TimestampTolerance(t *testing.T) {
	body := []byte(`{}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tolerance := 5 * time.Minute

	stale := now.Add(-6 * time.Minute)
	if err := Verify(testKey, testID, stale, Sign(testKey, testID, stale, body), body, tolerance, now); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("stale timestamp error = %v, want ErrTimestampOutOfRange", err)
	}
	future := now.Add(6 * time.Minute)
	if err := Verify(testKey, testID, future, Sign(testKey, testID, future, body), body, tolerance, now); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("future timestamp error = %v, want ErrTimestampOutOfRange", err)
	}
	// Boundary is inclusive.
	boundary := now.Add(-tolerance)
	if err := Verify(testKey, testID, boundary, Sign(testKey, testID, boundary, body), body, tolerance, now); err != nil {
		t.Fatalf("boundary timestamp rejected: %v", err)
	}
}

func TestVerify_MalformedHeaders(t *testing.T) {
	body := []byte(`{}`)
	now := time.Now().UTC()
	valid := Sign(testKey, testID, now, body)
	encoded := strings.TrimPrefix(valid, "v1,")

	cases := []struct {
		name      string
		signature string
		want      error
	}{
		{"empty", "", ErrMissingSignature},
		{"unknown version only", "v1a," + encoded, ErrMissingSignature},
		{"legacy v1= form", "v1=" + strings.Repeat("0", 64), ErrMissingSignature},
		{"bad base64", "v1,!!!!", ErrMissingSignature},
		{"short digest", "v1," + base64.StdEncoding.EncodeToString([]byte("abcd")), ErrMissingSignature},
		{"wrong value", "v1," + base64.StdEncoding.EncodeToString(make([]byte, 32)), ErrInvalidSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(testKey, testID, now, tc.signature, body, time.Minute, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify() error = %v, want %v", err, tc.want)
			}
		})
	}

	if err := Verify(nil, testID, now, valid, body, time.Minute, now); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("empty key error = %v, want ErrEmptySecret", err)
	}
}

// During a secret rotation the sender lists several space-separated
// signatures; one match is enough, and unknown versions are skipped.
func TestVerify_MultipleSignaturesDuringRotation(t *testing.T) {
	now := time.Now().UTC()
	body := []byte(`{"ok":true}`)
	oldKey := []byte("old-key-old-key-old-key-old-key!")

	header := SignatureHeader(
		"v1a,c29tZS1hc3ltbWV0cmljLXNpZ25hdHVyZQ==",
		Sign(oldKey, testID, now, body),
		Sign(testKey, testID, now, body),
	)
	if strings.Count(header, " ") != 2 {
		t.Fatalf("header %q must be space-separated", header)
	}
	for name, key := range map[string][]byte{"old": oldKey, "new": testKey} {
		if err := Verify(key, testID, now, header, body, time.Minute, now); err != nil {
			t.Fatalf("Verify(%s key) should accept one matching candidate: %v", name, err)
		}
	}
}

func TestVerifyRequest(t *testing.T) {
	now := time.Now().UTC()
	body := []byte(`{"x":1}`)
	headers := http.Header{}
	headers.Set(HeaderID, testID)
	headers.Set(HeaderTimestamp, "not-a-number")
	headers.Set(HeaderSignature, Sign(testKey, testID, now, body))

	if err := VerifyRequest(testKey, headers, body, time.Minute, now); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("invalid timestamp error = %v, want ErrInvalidTimestamp", err)
	}
	headers.Set(HeaderTimestamp, now.Format(time.RFC3339))
	if err := VerifyRequest(testKey, headers, body, time.Minute, now); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("RFC3339 timestamp must be rejected, got %v", err)
	}
	headers.Del(HeaderID)
	if err := VerifyRequest(testKey, headers, body, time.Minute, now); !errors.Is(err, ErrMissingID) {
		t.Fatalf("missing id error = %v, want ErrMissingID", err)
	}
}

func TestVerifyRequest_Valid(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"x":1}`)
	headers := http.Header{}
	headers.Set(HeaderID, testID)
	headers.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	headers.Set(HeaderSignature, Sign(testKey, testID, now, body))

	if err := VerifyRequest(testKey, headers, body, time.Minute, now); err != nil {
		t.Fatalf("VerifyRequest() rejected valid headers: %v", err)
	}
	// The id header is signed: changing it invalidates the signature.
	headers.Set(HeaderID, "msg_replayed")
	if err := VerifyRequest(testKey, headers, body, time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("replayed id error = %v, want ErrInvalidSignature", err)
	}
}

// TestVerify_FarTimestampsDoNotOverflow guards the skew check: a timestamp
// ~292+ years from now saturates time.Duration, and negating math.MinInt64
// stays negative, so an abs(now-ts) check would accept it under any
// tolerance. Each signature here is valid for its timestamp.
func TestVerify_FarTimestampsDoNotOverflow(t *testing.T) {
	body := []byte(`{}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		timestamp time.Time
	}{
		{"milliseconds as seconds", time.Unix(now.UnixMilli(), 0).UTC()},
		{"max int64 seconds", time.Unix(math.MaxInt64, 0).UTC()},
		{"min int64 seconds", time.Unix(math.MinInt64, 0).UTC()},
		{"500 years ahead", now.AddDate(500, 0, 0)},
		{"500 years ago", now.AddDate(-500, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signature := Sign(testKey, testID, tc.timestamp, body)
			err := Verify(testKey, testID, tc.timestamp, signature, body, time.Minute, now)
			if !errors.Is(err, ErrTimestampOutOfRange) {
				t.Fatalf("Verify() error = %v, want ErrTimestampOutOfRange", err)
			}
		})
	}
}

func TestVerifyRequest_RejectsImplausibleTimestamps(t *testing.T) {
	body := []byte(`{}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"zero", "0", ErrInvalidTimestamp},
		{"negative", "-1", ErrInvalidTimestamp},
		{"min int64", strconv.FormatInt(math.MinInt64, 10), ErrInvalidTimestamp},
		{"max int64", strconv.FormatInt(math.MaxInt64, 10), ErrInvalidTimestamp},
		{"milliseconds", strconv.FormatInt(now.UnixMilli(), 10), ErrInvalidTimestamp},
		{"year 3000 seconds", strconv.FormatInt(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 10), ErrTimestampOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seconds, err := strconv.ParseInt(tc.raw, 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.raw, err)
			}
			headers := http.Header{}
			headers.Set(HeaderID, testID)
			headers.Set(HeaderTimestamp, tc.raw)
			headers.Set(HeaderSignature, Sign(testKey, testID, time.Unix(seconds, 0), body))
			if err := VerifyRequest(testKey, headers, body, time.Minute, now); !errors.Is(err, tc.want) {
				t.Fatalf("VerifyRequest() error = %v, want %v", err, tc.want)
			}
		})
	}
}
