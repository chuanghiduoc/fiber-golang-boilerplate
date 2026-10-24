package webhook

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	secret := []byte("whsec_test_secret")
	body := []byte(`{"id":"evt_1","type":"file.uploaded"}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	signature := Sign(secret, now, body)
	if len(signature) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars", len(signature))
	}
	if err := Verify(secret, now, SignatureHeader(signature), body, time.Minute, now); err != nil {
		t.Fatalf("Verify() rejected a valid signature: %v", err)
	}

	if err := Verify(secret, now, SignatureHeader(signature), []byte(`tampered`), time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Verify() tampered body error = %v, want ErrInvalidSignature", err)
	}
	if err := Verify([]byte("wrong"), now, SignatureHeader(signature), body, time.Minute, now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Verify() wrong secret error = %v, want ErrInvalidSignature", err)
	}
}

func TestVerify_TimestampTolerance(t *testing.T) {
	secret := []byte("secret")
	body := []byte(`{}`)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tolerance := 5 * time.Minute
	signature := SignatureHeader(Sign(secret, now.Add(-6*time.Minute), body))

	err := Verify(secret, now.Add(-6*time.Minute), signature, body, tolerance, now)
	if !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("stale timestamp error = %v, want ErrTimestampOutOfRange", err)
	}

	future := now.Add(6 * time.Minute)
	signature = SignatureHeader(Sign(secret, future, body))
	err = Verify(secret, future, signature, body, tolerance, now)
	if !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("future timestamp error = %v, want ErrTimestampOutOfRange", err)
	}

	// Boundary is inclusive.
	boundary := now.Add(-tolerance)
	signature = SignatureHeader(Sign(secret, boundary, body))
	if err := Verify(secret, boundary, signature, body, tolerance, now); err != nil {
		t.Fatalf("boundary timestamp rejected: %v", err)
	}
}

func TestVerify_MalformedHeaders(t *testing.T) {
	secret := []byte("secret")
	body := []byte(`{}`)
	now := time.Now().UTC()
	valid := Sign(secret, now, body)

	cases := []struct {
		name      string
		signature string
		want      error
	}{
		{"empty", "", ErrMissingSignature},
		{"wrong scheme", "v2=" + valid, ErrMissingSignature},
		{"bad hex", "v1=zzzz", ErrMissingSignature},
		{"short", "v1=abcd", ErrMissingSignature},
		{"bad value", "v1=" + strings.Repeat("0", 64), ErrInvalidSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(secret, now, tc.signature, body, time.Minute, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify() error = %v, want %v", err, tc.want)
			}
		})
	}

	if err := Verify(nil, now, SignatureHeader(valid), body, time.Minute, now); !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("empty secret error = %v, want ErrEmptySecret", err)
	}
}

func TestVerify_MultipleSignaturesDuringRotation(t *testing.T) {
	secret := []byte("new-secret")
	now := time.Now().UTC()
	body := []byte(`{"ok":true}`)
	newSig := Sign(secret, now, body)
	oldSig := strings.Repeat("a", 64)

	header := "v1=" + oldSig + ", v1=" + newSig
	if err := Verify(secret, now, header, body, time.Minute, now); err != nil {
		t.Fatalf("Verify() should accept one matching candidate: %v", err)
	}
}

func TestVerifyRequest(t *testing.T) {
	secret := []byte("secret")
	now := time.Now().UTC()
	body := []byte(`{"x":1}`)
	headers := http.Header{}
	headers.Set(HeaderTimestamp, "not-a-number")
	headers.Set(HeaderSignature, SignatureHeader(Sign(secret, now, body)))

	if err := VerifyRequest(secret, headers, body, time.Minute, now); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("invalid timestamp error = %v, want ErrInvalidTimestamp", err)
	}

	headers.Set(HeaderTimestamp, now.Format(time.RFC3339))
	if err := VerifyRequest(secret, headers, body, time.Minute, now); !errors.Is(err, ErrInvalidTimestamp) {
		t.Fatalf("RFC3339 timestamp must be rejected, got %v", err)
	}
}

func TestVerifyRequest_Valid(t *testing.T) {
	secret := []byte("secret")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"x":1}`)
	headers := http.Header{}
	headers.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	headers.Set(HeaderSignature, SignatureHeader(Sign(secret, now, body)))

	if err := VerifyRequest(secret, headers, body, time.Minute, now); err != nil {
		t.Fatalf("VerifyRequest() rejected valid headers: %v", err)
	}
}
