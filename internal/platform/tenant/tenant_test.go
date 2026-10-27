package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestIDFromContext_ZeroPropagates(t *testing.T) {
	ctx := WithID(context.Background(), uuid.Nil)
	if got := IDFromContext(ctx); got != uuid.Nil {
		t.Errorf("IDFromContext(WithID(uuid.Nil)) = %s, want nil UUID", got)
	}

	ctx = WithIdentity(context.Background(), testID(2), "acme")
	if got := IDFromContext(ctx); got != testID(2) {
		t.Errorf("IDFromContext(WithIdentity) = %s, want %s", got, testID(2))
	}
	if got := SlugFromContext(ctx); got != "acme" {
		t.Errorf("SlugFromContext = %q, want acme", got)
	}
}

func TestRequire(t *testing.T) {
	_, err := Require(WithID(context.Background(), uuid.Nil))
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("Require(WithID(uuid.Nil)) error = %v, want ErrMissingTenant", err)
	}

	got, err := Require(WithID(context.Background(), testID(7)))
	if err != nil {
		t.Fatalf("Require(WithID(7)) error = %v", err)
	}
	if got != testID(7) {
		t.Fatalf("Require(WithID(7)) = %s, want %s", got, testID(7))
	}

	// The seeded default tenant is an explicit tenant like any other.
	got, err = Require(WithID(context.Background(), DefaultID))
	if err != nil || got != DefaultID {
		t.Fatalf("Require(WithID(DefaultID)) = %s, %v", got, err)
	}
}
