package cache

import (
	"testing"

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
