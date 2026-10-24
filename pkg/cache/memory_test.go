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

func TestSetNX(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	if stored, err := mc.SetNX(ctx, "lock", []byte("owner-1"), 20*time.Millisecond); err != nil || !stored {
		t.Fatalf("SetNX(absent) = %v, %v; want true, nil", stored, err)
	}
	if stored, err := mc.SetNX(ctx, "lock", []byte("owner-2"), time.Minute); err != nil || stored {
		t.Fatalf("SetNX(held) = %v, %v; want false, nil", stored, err)
	}
	if got, _ := mc.Get(ctx, "lock"); !bytes.Equal(got, []byte("owner-1")) {
		t.Fatalf("lock value = %q, want the first owner", got)
	}

	time.Sleep(30 * time.Millisecond)
	if stored, err := mc.SetNX(ctx, "lock", []byte("owner-2"), time.Minute); err != nil || !stored {
		t.Fatalf("SetNX(expired) = %v, %v; want true, nil", stored, err)
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

func TestIncrementRejectsNonIntegerValue(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	if err := mc.Set(ctx, "token", []byte("owner-token"), time.Minute); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if _, err := mc.Increment(ctx, "token", time.Minute); err == nil {
		t.Fatal("Increment should reject a non-integer value")
	}
	if value, _ := mc.Get(ctx, "token"); string(value) != "owner-token" {
		t.Fatalf("token = %q, want it untouched", value)
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

func TestCompareAndDelete(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	if err := mc.Set(ctx, "lock", []byte("owner-1"), time.Minute); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	deleted, err := mc.CompareAndDelete(ctx, "lock", []byte("owner-2"))
	if err != nil {
		t.Fatalf("CompareAndDelete(stale owner) failed: %v", err)
	}
	if deleted {
		t.Fatal("CompareAndDelete deleted a lock owned by a different token")
	}
	if value, _ := mc.Get(ctx, "lock"); string(value) != "owner-1" {
		t.Fatalf("lock value = %q, want owner-1", value)
	}

	deleted, err = mc.CompareAndDelete(ctx, "lock", []byte("owner-1"))
	if err != nil {
		t.Fatalf("CompareAndDelete(owner) failed: %v", err)
	}
	if !deleted {
		t.Fatal("CompareAndDelete did not delete the matching owner")
	}
	if value, _ := mc.Get(ctx, "lock"); value != nil {
		t.Fatalf("lock survived deletion: %q", value)
	}

	deleted, err = mc.CompareAndDelete(ctx, "missing", []byte("owner-1"))
	if err != nil {
		t.Fatalf("CompareAndDelete(missing) failed: %v", err)
	}
	if deleted {
		t.Fatal("CompareAndDelete reported deleting a missing key")
	}
}

func TestCompareAndDeleteExpired(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()
	ctx := context.Background()

	if err := mc.Set(ctx, "lock", []byte("owner-1"), time.Millisecond); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	deleted, err := mc.CompareAndDelete(ctx, "lock", []byte("owner-1"))
	if err != nil {
		t.Fatalf("CompareAndDelete failed: %v", err)
	}
	if deleted {
		t.Fatal("CompareAndDelete deleted an expired key")
	}
}

func TestCompareDeleterImplementations(t *testing.T) {
	var _ CompareDeleter = (*MemoryCache)(nil)
	var _ CompareDeleter = (*RedisCache)(nil)
}

func TestPing(t *testing.T) {
	mc := NewMemoryCache()
	defer mc.Close()

	if err := mc.Ping(context.Background()); err != nil {
		t.Errorf("Ping returned error: %v", err)
	}
}
