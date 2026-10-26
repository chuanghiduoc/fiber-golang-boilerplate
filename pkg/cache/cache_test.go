package cache

import (
	"context"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func TestNewCache_Memory(t *testing.T) {
	c, err := NewCache(config.CacheConfig{Driver: "memory"})
	if err != nil {
		t.Fatalf("NewCache(memory) returned error: %v", err)
	}
	if c == nil {
		t.Fatal("NewCache(memory) returned nil")
	}
	_ = c.Close()
}

func TestNewCache_MemoryUsesConfiguredLimit(t *testing.T) {
	c, err := NewCache(config.CacheConfig{Driver: "memory", MemoryMaxEntries: 2})
	if err != nil {
		t.Fatalf("NewCache(memory) returned error: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		if err := c.Set(ctx, key, []byte(key), time.Minute); err != nil {
			t.Fatalf("Set(%s): %v", key, err)
		}
	}
	if got, _ := c.Get(ctx, "a"); got != nil {
		t.Fatalf("CACHE_MEMORY_MAX_ENTRIES was not applied: a = %q, want evicted", got)
	}
}

func TestNewCache_UnknownDriver(t *testing.T) {
	c, err := NewCache(config.CacheConfig{Driver: "unknown"})
	if err == nil {
		_ = c.Close()
		t.Fatal("NewCache(unknown) should return error for unsupported driver")
	}
	if c != nil {
		t.Fatal("NewCache(unknown) should return nil cache")
	}
}
