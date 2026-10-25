package keyring

import (
	"errors"
	"testing"
)

const info = "test/keyring/v1"

func TestNewRejectsEmptyActiveSecret(t *testing.T) {
	for _, secret := range []string{"", "   "} {
		if _, err := New(info, secret); !errors.Is(err, ErrSecretRequired) {
			t.Fatalf("New(%q) error = %v, want ErrSecretRequired", secret, err)
		}
	}
}

func TestRingOrdersAndDeduplicatesKeys(t *testing.T) {
	ring, err := New(info, "active-secret", "old-secret", "", "active-secret", "old-secret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(ring.Keys()); got != 2 {
		t.Fatalf("Keys() = %d, want 2 (empty and duplicate secrets ignored)", got)
	}
	if ring.Active().ID != ID(info, "active-secret") {
		t.Fatal("the active key must be first")
	}
	if _, ok := ring.Lookup(ID(info, "old-secret")); !ok {
		t.Fatal("previous key must be found by id")
	}
	if _, ok := ring.Lookup("deadbeef"); ok {
		t.Fatal("unknown id must not resolve")
	}
}

func TestIDIsStableAndDomainSeparated(t *testing.T) {
	first, second := ID(info, "s"), ID(info, "s")
	if first != second {
		t.Fatal("ID must be deterministic")
	}
	if ID(info, "s") == ID("other/domain", "s") {
		t.Fatal("ID must depend on the domain")
	}
	if len(ID(info, "s")) != idLength {
		t.Fatalf("ID length = %d, want %d", len(ID(info, "s")), idLength)
	}
}
