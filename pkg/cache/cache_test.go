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

func TestNewCache_Default(t *testing.T) {
	c, err := NewCache(config.CacheConfig{Driver: "unknown"})
	if err != nil {
		t.Fatalf("NewCache(unknown) returned error: %v", err)
	}
	if c == nil {
		t.Fatal("NewCache(unknown) returned nil")
	}
	_ = c.Close()
}
