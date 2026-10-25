package tenant

import (
	"context"
	"testing"
)

func TestWithID_ZeroFallsBackAndNegativePropagates(t *testing.T) {
	ctx := WithID(context.Background(), 0)
	if got := EffectiveID(ctx); got != DefaultID {
		t.Errorf("EffectiveID(WithID(0)) = %d, want %d", got, DefaultID)
	}

	// A negative ID is visible to validation boundaries; it must not be
	// silently rewritten to the compatibility tenant.
	ctx = WithID(context.Background(), -1)
	if got := IDFromContext(ctx); got != -1 {
		t.Errorf("IDFromContext(WithID(-1)) = %d, want -1", got)
	}
	if got := EffectiveID(ctx); got != -1 {
		t.Errorf("EffectiveID(WithID(-1)) = %d, want -1", got)
	}

	ctx = WithIdentity(context.Background(), 2, "acme")
	if got := EffectiveID(ctx); got != 2 {
		t.Errorf("EffectiveID(WithIdentity) = %d, want 2", got)
	}
	if got := SlugFromContext(ctx); got != "acme" {
		t.Errorf("SlugFromContext = %q, want acme", got)
	}
}
