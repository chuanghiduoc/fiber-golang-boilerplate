package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// CompareDeleter is an optional capability for compare-and-delete operations.
// It is separate from Cache so existing implementations and test doubles do not
// need to provide it. Callers must type-assert before use.
type CompareDeleter interface {
	CompareAndDelete(ctx context.Context, key string, value []byte) (bool, error)
}

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// SetNX atomically stores value with ttl only when key is absent or expired
	// and reports whether it was stored. It is the primitive for owner-token
	// locks: the value is the owner's token, so a caller whose reply was lost can
	// read the key back and recognise its own claim.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
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
		return NewMemoryCacheWithLimit(cfg.MemoryMaxEntries), nil
	default:
		return nil, fmt.Errorf("unsupported cache driver: %q (supported: memory, redis)", cfg.Driver)
	}
}
