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

func TestPing(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()

	if err := mc.Ping(context.Background()); err != nil {
		t.Errorf("Ping returned error: %v", err)
	}
}
