package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// ErrFull is returned by a bounded cache when a write cannot be stored because
// every live entry is non-evictable (a SetNX lock or an Increment counter).
// Evicting those entries would silently release a lock, reset a lockout, or
// admit a request past its rate limit, so the write fails loudly instead.
// Callers treat it like any other cache error: login lockout fails closed,
// rate limiting and idempotency fail open.
var ErrFull = errors.New("cache: full of live non-evictable entries")

// CompareDeleter is an optional capability for compare-and-delete operations.
// It is separate from Cache so existing implementations and test doubles do not
// need to provide it. Callers must type-assert before use.
type CompareDeleter interface {
	CompareAndDelete(ctx context.Context, key string, value []byte) (bool, error)
}

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	// Set stores a cached value. A bounded cache may reclaim expired entries or
	// evict other Set values to make room, and returns ErrFull when every live
	// entry is a non-evictable lock or counter.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// SetNX atomically stores value with ttl only when key is absent or expired
	// and reports whether it was stored. It is the primitive for owner-token
	// locks: the value is the owner's token, so a caller whose reply was lost can
	// read the key back and recognise its own claim. The entry is never evicted
	// while live; a full cache of non-evictable entries returns ErrFull.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	// Increment atomically increments the integer counter at key and returns the
	// new value. On the first increment (key absent/expired) the counter starts
	// at 1 and ttl is applied. Atomicity prevents concurrent requests from racing
	// a read-modify-write (e.g. bypassing brute-force lockout in parallel). The
	// counter is never evicted while live; a full cache of non-evictable entries
	// returns ErrFull for a new key.
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// IncrementBelow atomically increments the integer counter at key and
	// returns the new value only while the result is at most maxValue. When the
	// counter has already reached maxValue it is left unchanged and ok is false;
	// the current value is returned so callers can report headroom. Rejected
	// operations therefore never create or advance a counter, which keeps a
	// sliding-window limiter's rejected retries from extending a block. A new
	// counter returns ErrFull when the cache has no evictable entry to reclaim.
	IncrementBelow(ctx context.Context, key string, ttl time.Duration, maxValue int64) (value int64, ok bool, err error)
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
