package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var testBinding = EndpointSecretBinding(testID(1), testID(2))

func TestSecretCodec_RoundTrip(t *testing.T) {
	codec, err := NewSecretCodec("dedicated-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	sealed, err := codec.Seal(testBinding, []byte("whsec_secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "whsec_secret") {
		t.Fatal("ciphertext must not contain plaintext")
	}
	plain, err := codec.Open(testBinding, sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(plain) != "whsec_secret" {
		t.Fatalf("plaintext = %q", plain)
	}

	// A fresh seal uses a fresh nonce.
	again, err := codec.Seal(testBinding, []byte("whsec_secret"))
	if err != nil {
		t.Fatalf("Seal again: %v", err)
	}
	if again == sealed {
		t.Fatal("two seals should not produce identical envelopes")
	}
}

func TestSecretCodec_WrongKeyAndTamper(t *testing.T) {
	codec, _ := NewSecretCodec("key-one")
	sealed, _ := codec.Seal(testBinding, codecTestSecret)

	other, _ := NewSecretCodec("key-two")
	if _, err := other.Open(testBinding, sealed); !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("wrong key error = %v, want ErrSecretDecryption", err)
	}
	if _, err := codec.Open(testBinding, sealed+"x"); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("tampered envelope error = %v, want ErrInvalidSecretEnvelope", err)
	}
	if _, err := codec.Open(testBinding, ""); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("empty envelope error = %v, want ErrInvalidSecretEnvelope", err)
	}
}

func TestSecretCodec_EnvelopeNamesActiveKey(t *testing.T) {
	codec, _ := NewSecretCodec("key-one")
	sealed, _ := codec.Seal(testBinding, codecTestSecret)
	var envelope secretEnvelope
	if err := json.Unmarshal([]byte(sealed), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Version != secretEnvelopeVer || envelope.KeyID != codec.ring.Active().ID || len(envelope.KeyID) != 8 {
		t.Fatalf("envelope = %+v, want v%d with the active key id", envelope, secretEnvelopeVer)
	}
}

func TestSecretCodec_BindingPreventsSwap(t *testing.T) {
	codec, _ := NewSecretCodec("key-one")
	sealed, err := codec.Seal(EndpointSecretBinding(testID(7), testID(41)), []byte("endpoint-secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	_, err = codec.Open(EndpointSecretBinding(testID(7), testID(42)), sealed)
	if !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("swapped endpoint error = %v, want ErrSecretDecryption", err)
	}
	if _, err := codec.Open(EndpointSecretBinding(testID(8), testID(41)), sealed); !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("swapped tenant error = %v, want ErrSecretDecryption", err)
	}
	if _, err := codec.Open(InboundSourceSecretBinding(testID(7), testID(41)), sealed); !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("swapped purpose error = %v, want ErrSecretDecryption", err)
	}
	plain, err := codec.Open(EndpointSecretBinding(testID(7), testID(41)), sealed)
	if err != nil || string(plain) != "endpoint-secret" {
		t.Fatalf("original binding = %q, %v", plain, err)
	}
}

func TestSecretCodec_RequiresCompleteBinding(t *testing.T) {
	codec, _ := NewSecretCodec("key-one")
	for _, binding := range []SecretBinding{
		{},
		{Purpose: PurposeEndpointSecret, TenantID: testID(1)},
		{Purpose: PurposeEndpointSecret, EntityID: testID(1)},
		{TenantID: testID(1), EntityID: testID(1)},
	} {
		if _, err := codec.Seal(binding, codecTestSecret); !errors.Is(err, ErrInvalidSecretBinding) {
			t.Errorf("Seal(%+v) error = %v, want ErrInvalidSecretBinding", binding, err)
		}
	}
	sealed, _ := codec.Seal(testBinding, codecTestSecret)
	if _, err := codec.Open(SecretBinding{}, sealed); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("Open without a binding error = %v, want ErrInvalidSecretEnvelope", err)
	}
}

func TestSecretCodec_RotationKeepsOldSecretsReadable(t *testing.T) {
	old, _ := NewSecretCodec("key-one")
	sealedOld, _ := old.Seal(testBinding, []byte("old-secret"))

	rotated, err := NewSecretCodec("key-two", "key-one")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	plain, err := rotated.Open(testBinding, sealedOld)
	if err != nil || string(plain) != "old-secret" {
		t.Fatalf("Open with previous key = %q, %v; want old-secret", plain, err)
	}

	sealedNew, _ := rotated.Seal(testBinding, []byte("new-secret"))
	if _, err := old.Open(testBinding, sealedNew); !errors.Is(err, ErrSecretDecryption) {
		t.Fatalf("codec without the new key: err = %v, want ErrSecretDecryption", err)
	}

	missingKID := strings.Replace(sealedNew, `"kid":"`+rotated.ring.Active().ID+`",`, "", 1)
	if _, err := rotated.Open(testBinding, missingKID); !errors.Is(err, ErrInvalidSecretEnvelope) {
		t.Fatalf("v3 envelope without kid: err = %v, want ErrInvalidSecretEnvelope", err)
	}
}

// TestSecretCodec_RejectsUnboundEnvelopes proves the pre-v3 envelopes whose
// AAD had no tenant/entity binding are not opened, even with the right key: a
// ciphertext copied between tenants or rows must never decrypt.
func TestSecretCodec_RejectsUnboundEnvelopes(t *testing.T) {
	old, _ := NewSecretCodec("key-one")
	aead := old.ring.Active().AEAD

	nonce := make([]byte, aead.NonceSize())
	unboundV1, _ := json.Marshal(secretEnvelope{
		Version: 1,
		Nonce:   base64.RawURLEncoding.EncodeToString(nonce),
		Data:    base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, []byte("legacy-secret"), []byte(secretKeyInfo))),
	})
	keyedNonce := make([]byte, aead.NonceSize())
	keyedAAD := []byte(secretKeyInfo + "|v2|" + old.ring.Active().ID)
	keyedV2, _ := json.Marshal(secretEnvelope{
		Version: 2,
		KeyID:   old.ring.Active().ID,
		Nonce:   base64.RawURLEncoding.EncodeToString(keyedNonce),
		Data:    base64.RawURLEncoding.EncodeToString(aead.Seal(nil, keyedNonce, []byte("keyed-secret"), keyedAAD)),
	})

	rotated, err := NewSecretCodec("key-two", "key-one")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	for name, envelope := range map[string]string{
		"v1 without binding": string(unboundV1),
		"v2 without binding": string(keyedV2),
	} {
		if _, err := rotated.Open(testBinding, envelope); !errors.Is(err, ErrInvalidSecretEnvelope) {
			t.Fatalf("%s: error = %v, want ErrInvalidSecretEnvelope", name, err)
		}
	}
}

func TestSecretCodec_RequiresKey(t *testing.T) {
	for _, key := range []string{"", "   "} {
		if _, err := NewSecretCodec(key); !errors.Is(err, ErrSecretKeyRequired) {
			t.Fatalf("NewSecretCodec(%q) error = %v", key, err)
		}
	}
	var nilCodec *SecretCodec
	if _, err := nilCodec.Seal(testBinding, nil); !errors.Is(err, ErrNilSecretCodec) {
		t.Fatalf("nil Seal error = %v", err)
	}
	if _, err := nilCodec.Open(testBinding, "x"); !errors.Is(err, ErrNilSecretCodec) {
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

// codecTestSecret is a runtime-built fixture secret, not committed key
// material.
var codecTestSecret = []byte(strings.Join([]string{"codec", "fixture", "secret"}, "-"))
