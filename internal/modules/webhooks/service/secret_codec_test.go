package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSecretCodec_RoundTrip(t *testing.T) {
	codec, err := NewSecretCodec("dedicated-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	sealed, err := codec.Seal([]byte("whsec_secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "whsec_secret") {
		t.Fatal("ciphertext must not contain plaintext")
	}
	plain, err := codec.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(plain) != "whsec_secret" {
		t.Fatalf("plaintext = %q", plain)
	}

	// A fresh seal uses a fresh nonce.
	again, err := codec.Seal([]byte("whsec_secret"))
	if err != nil {
		t.Fatalf("Seal again: %v", err)
	}
	if again == sealed {
		t.Fatal("two seals should not produce identical envelopes")
	}
}

func TestSecretCodec_WrongKeyAndTamper(t *testing.T) {
	codec, _ := NewSecretCodec("key-one")
	sealed, _ := codec.Seal([]byte("secret"))

	other, _ := NewSecretCodec("key-two")
	if _, err := other.Open(sealed); !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("wrong key error = %v, want ErrSecretDecryption", err)
	}
	if _, err := codec.Open(sealed + "x"); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("tampered envelope error = %v, want ErrInvalidSecretEnvelope", err)
	}
	if _, err := codec.Open(""); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("empty envelope error = %v, want ErrInvalidSecretEnvelope", err)
	}
}

func TestSecretCodec_EnvelopeNamesActiveKey(t *testing.T) {
	codec, _ := NewSecretCodec("key-one")
	sealed, _ := codec.Seal([]byte("secret"))
	var envelope secretEnvelope
	if err := json.Unmarshal([]byte(sealed), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Version != secretEnvelopeVer || envelope.KeyID != codec.ring.Active().ID || len(envelope.KeyID) != 8 {
		t.Fatalf("envelope = %+v, want v%d with the active key id", envelope, secretEnvelopeVer)
	}
}

func TestSecretCodec_RotationKeepsOldSecretsReadable(t *testing.T) {
	old, _ := NewSecretCodec("key-one")
	sealedOld, _ := old.Seal([]byte("old-secret"))

	// A version 1 envelope (no key id), as stored before key ids existed.
	aead := old.ring.Active().AEAD
	nonce := make([]byte, aead.NonceSize())
	legacy, _ := json.Marshal(secretEnvelope{
		Version: legacySecretEnvelopeVer,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, []byte("legacy-secret"), []byte(secretKeyInfo))),
	})

	rotated, err := NewSecretCodec("key-two", "key-one")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	for envelope, want := range map[string]string{sealedOld: "old-secret", string(legacy): "legacy-secret"} {
		plain, err := rotated.Open(envelope)
		if err != nil || string(plain) != want {
			t.Fatalf("Open with previous key = %q, %v; want %q", plain, err, want)
		}
	}

	sealedNew, _ := rotated.Seal([]byte("new-secret"))
	if _, err := old.Open(sealedNew); !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("codec without the new key: err = %v, want ErrSecretDecryption", err)
	}

	missingKID := strings.Replace(sealedNew, `"kid":"`+rotated.ring.Active().ID+`",`, "", 1)
	if _, err := rotated.Open(missingKID); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("v2 envelope without kid: err = %v, want ErrInvalidSecretEnvelope", err)
	}
}

func TestSecretCodec_RequiresKey(t *testing.T) {
	for _, key := range []string{"", "   "} {
		if _, err := NewSecretCodec(key); !errors.Is(err, ErrSecretKeyRequired) {
			t.Fatalf("NewSecretCodec(%q) error = %v", key, err)
		}
	}
	var nilCodec *SecretCodec
	if _, err := nilCodec.Seal(nil); !errors.Is(err, ErrNilSecretCodec) {
		t.Fatalf("nil Seal error = %v", err)
	}
	if _, err := nilCodec.Open("x"); !errors.Is(err, ErrNilSecretCodec) {
		t.Fatalf("nil Open error = %v", err)
	}
}

func TestNormalizeEventTypes(t *testing.T) {
	got, err := normalizeEventTypes(nil)
	if err != nil || len(got) != 1 || got[0] != "*" {
		t.Fatalf("empty patterns = %v, %v", got, err)
	}

	got, err = normalizeEventTypes([]string{" file.* ", "file.*", "file.uploaded"})
	if err != nil {
		t.Fatalf("normalizeEventTypes: %v", err)
	}
	if len(got) != 2 || got[0] != "file.*" || got[1] != "file.uploaded" {
		t.Fatalf("patterns = %v", got)
	}

	for _, invalid := range [][]string{{"Bad"}, {"file uploaded"}, {"file.*.x"}} {
		if _, err := normalizeEventTypes(invalid); err == nil {
			t.Errorf("normalizeEventTypes(%v) should fail", invalid)
		}
	}
}

func TestValidateSlugAndCursor(t *testing.T) {
	for _, slug := range []string{"github", "acme-corp", "a1-b2"} {
		if err := validateSlug(slug); err != nil {
			t.Errorf("validateSlug(%q): %v", slug, err)
		}
	}
	for _, slug := range []string{"", "Acme", "has space", strings.Repeat("a", 64)} {
		if err := validateSlug(slug); err == nil {
			t.Errorf("validateSlug(%q) should fail", slug)
		}
	}

	cur, pageSize, err := buildCursor(1000, "")
	if err != nil {
		t.Fatalf("buildCursor: %v", err)
	}
	if pageSize != 100 || cur.Limit != 101 {
		t.Fatalf("pageSize=%d limit=%d", pageSize, cur.Limit)
	}
	if _, _, err := buildCursor(10, "not-a-cursor"); err == nil {
		t.Fatal("invalid cursor must fail")
	}
}
