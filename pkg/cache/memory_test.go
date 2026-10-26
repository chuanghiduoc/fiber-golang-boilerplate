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

func TestMemoryCacheEvictsLeastRecentlyUsed(t *testing.T) {
	mc := NewMemoryCacheWithLimit(2)
	defer mc.Close()
	ctx := context.Background()

	if err := mc.Set(ctx, "a", []byte("a"), time.Minute); err != nil {
		t.Fatalf("Set(a): %v", err)
	}
	if err := mc.Set(ctx, "b", []byte("b"), time.Minute); err != nil {
		t.Fatalf("Set(b): %v", err)
	}
	// Reading a makes b the least recently used entry.
	if got, _ := mc.Get(ctx, "a"); string(got) != "a" {
		t.Fatalf("Get(a) = %q, want a", got)
	}
	if err := mc.Set(ctx, "c", []byte("c"), time.Minute); err != nil {
		t.Fatalf("Set(c): %v", err)
	}

	if got, _ := mc.Get(ctx, "b"); got != nil {
		t.Fatalf("b = %q, want it evicted as the least recently used entry", got)
	}
	if exists, _ := mc.Exists(ctx, "a"); !exists {
		t.Fatal("a was evicted but was recently used")
	}
	if exists, _ := mc.Exists(ctx, "c"); !exists {
		t.Fatal("the newly written entry was evicted")
	}
}

func TestMemoryCacheDropsExpiredEntriesBeforeLRU(t *testing.T) {
	mc := NewMemoryCacheWithLimit(2)
	defer mc.Close()
	ctx := context.Background()

	if err := mc.Set(ctx, "live", []byte("live"), time.Minute); err != nil {
		t.Fatalf("Set(live): %v", err)
	}
	if err := mc.Set(ctx, "expired", []byte("old"), time.Millisecond); err != nil {
		t.Fatalf("Set(expired): %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	// The expired entry is the most recently used one, so eviction must reclaim
	// expiry before falling back to the LRU tail (which is the live entry).
	if err := mc.Set(ctx, "new", []byte("new"), time.Minute); err != nil {
		t.Fatalf("Set(new): %v", err)
	}
	if exists, _ := mc.Exists(ctx, "expired"); exists {
		t.Fatal("the expired entry survived eviction")
	}
	if exists, _ := mc.Exists(ctx, "live"); !exists {
		t.Fatal("a live entry was evicted before an expired one")
	}
}

func TestMemoryCacheEvictionKeepsSetNXExact(t *testing.T) {
	mc := NewMemoryCacheWithLimit(1)
	defer mc.Close()
	ctx := context.Background()

	stored, err := mc.SetNX(ctx, "lock", []byte("owner-1"), time.Minute)
	if err != nil || !stored {
		t.Fatalf("SetNX(lock) = %v, %v; want true, nil", stored, err)
	}
	// A held lock cannot be taken, and the rejected attempt must not evict the
	// holder (which would silently release the lock).
	stored, err = mc.SetNX(ctx, "lock", []byte("owner-2"), time.Minute)
	if err != nil || stored {
		t.Fatalf("SetNX(held) = %v, %v; want false, nil", stored, err)
	}
	if got, _ := mc.Get(ctx, "lock"); string(got) != "owner-1" {
		t.Fatalf("lock = %q, want owner-1", got)
	}

	// Filling the cache with another key evicts the lock as the LRU entry.
	if stored, err := mc.SetNX(ctx, "other", []byte("other"), time.Minute); err != nil || !stored {
		t.Fatalf("SetNX(other) = %v, %v; want true, nil", stored, err)
	}
	if deleted, err := mc.CompareAndDelete(ctx, "lock", []byte("owner-1")); err != nil || deleted {
		t.Fatalf("CompareAndDelete(evicted lock) = %v, %v; want false, nil", deleted, err)
	}
}

func TestMemoryCacheConcurrentSetNXHasOneWinner(t *testing.T) {
	mc := NewMemoryCacheWithLimit(16)
	defer mc.Close()
	ctx := context.Background()

	const contenders = 32
	results := make(chan bool, contenders)
	for range contenders {
		go func() {
			stored, err := mc.SetNX(ctx, "lock", []byte("owner"), time.Minute)
			if err != nil {
				t.Errorf("SetNX: %v", err)
			}
			results <- stored
		}()
	}
	winners := 0
	for range contenders {
		if <-results {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("SetNX winners = %d, want exactly 1", winners)
	}
}
