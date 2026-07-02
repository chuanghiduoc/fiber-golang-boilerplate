package cache

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestGetSet(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	if err := mc.Set(ctx, "key1", []byte("value1"), 5*time.Minute); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	got, err := mc.Get(ctx, "key1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !bytes.Equal(got, []byte("value1")) {
		t.Errorf("Get = %q, want %q", got, "value1")
	}
}

func TestGetMiss(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	got, err := mc.Get(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got != nil {
		t.Errorf("Get = %v, want nil", got)
	}
}

func TestDelete(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	_ = mc.Set(ctx, "key1", []byte("value1"), 5*time.Minute)
	if err := mc.Delete(ctx, "key1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	got, _ := mc.Get(ctx, "key1")
	if got != nil {
		t.Errorf("Get after Delete = %v, want nil", got)
	}
}

func TestExists(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	exists, err := mc.Exists(ctx, "missing")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if exists {
		t.Error("Exists should return false for missing key")
	}

	_ = mc.Set(ctx, "key1", []byte("value1"), 5*time.Minute)
	exists, err = mc.Exists(ctx, "key1")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if !exists {
		t.Error("Exists should return true for existing key")
	}
}

func TestTTLExpiry(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	_ = mc.Set(ctx, "ephemeral", []byte("data"), 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	got, _ := mc.Get(ctx, "ephemeral")
	if got != nil {
		t.Error("expired key should return nil")
	}

	exists, _ := mc.Exists(ctx, "ephemeral")
	if exists {
		t.Error("expired key should not exist")
	}
}

func TestZeroTTL(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	_ = mc.Set(ctx, "forever", []byte("data"), 0)
	time.Sleep(5 * time.Millisecond)

	got, _ := mc.Get(ctx, "forever")
	if got == nil {
		t.Error("zero TTL key should not expire")
	}
}

func TestClose(t *testing.T) {
	mc := NewMemoryCache()
	if err := mc.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	mc := NewMemoryCache()
	_ = mc.Close()
	// A second Close must not panic on a closed channel.
	if err := mc.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
}

func TestIncrement(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	for want := int64(1); want <= 3; want++ {
		got, err := mc.Increment(ctx, "counter", time.Minute)
		if err != nil {
			t.Fatalf("Increment failed: %v", err)
		}
		if got != want {
			t.Errorf("Increment = %d, want %d", got, want)
		}
	}

	// The stored value should be readable as the latest count.
	v, _ := mc.Get(ctx, "counter")
	if string(v) != "3" {
		t.Errorf("stored counter = %q, want %q", v, "3")
	}
}

func TestIncrementExpiryNotExtended(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	if _, err := mc.Increment(ctx, "c", 5*time.Millisecond); err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	// A later increment must keep the original short expiry, not reset it.
	if _, err := mc.Increment(ctx, "c", time.Hour); err != nil {
		t.Fatalf("Increment failed: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if got, _ := mc.Get(ctx, "c"); got != nil {
		t.Errorf("counter should have expired on its original window, got %q", got)
	}
}

func TestPing(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()

	if err := mc.Ping(context.Background()); err != nil {
		t.Errorf("Ping returned error: %v", err)
	}
}
