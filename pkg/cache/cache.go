package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	// Increment atomically increments the integer counter at key and returns the
	// new value. On the first increment (key absent/expired) the counter starts
	// at 1 and ttl is applied. Atomicity prevents concurrent requests from racing
	// a read-modify-write (e.g. bypassing brute-force lockout in parallel).
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
	Close() error
	Ping(ctx context.Context) error
}

func NewCache(cfg config.CacheConfig) (Cache, error) {
	switch cfg.Driver {
	case "redis":
		return NewRedisCache(cfg)
	case "memory":
		return NewMemoryCache(), nil
	default:
		return nil, fmt.Errorf("unsupported cache driver: %q (supported: memory, redis)", cfg.Driver)
	}
}
