package middleware

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

// cacheLimiterStorage adapts the application cache (memory or Redis) to the
// Fiber limiter storage interface. With Redis this makes rate-limit counters
// shared across replicas instead of per-process.
type cacheLimiterStorage struct {
	cache cache.Cache
}

// NewLimiterStorage returns nil when no cache is configured, which leaves the
// limiter's in-memory default in place.
func NewLimiterStorage(appCache cache.Cache) fiber.Storage {
	if appCache == nil {
		return nil
	}
	return &cacheLimiterStorage{cache: appCache}
}

func (s *cacheLimiterStorage) GetWithContext(ctx context.Context, key string) ([]byte, error) {
	return s.cache.Get(ctx, key)
}

func (s *cacheLimiterStorage) Get(key string) ([]byte, error) {
	return s.cache.Get(context.Background(), key)
}

func (s *cacheLimiterStorage) SetWithContext(ctx context.Context, key string, val []byte, exp time.Duration) error {
	return s.cache.Set(ctx, key, val, exp)
}

func (s *cacheLimiterStorage) Set(key string, val []byte, exp time.Duration) error {
	return s.cache.Set(context.Background(), key, val, exp)
}

func (s *cacheLimiterStorage) DeleteWithContext(ctx context.Context, key string) error {
	return s.cache.Delete(ctx, key)
}

func (s *cacheLimiterStorage) Delete(key string) error {
	return s.cache.Delete(context.Background(), key)
}

// Reset/Close are not used by the limiter at runtime. Scanning and deleting
// every application cache key would be dangerous, so they are no-ops.
func (s *cacheLimiterStorage) ResetWithContext(context.Context) error { return nil }
func (s *cacheLimiterStorage) Reset() error                           { return nil }
func (s *cacheLimiterStorage) Close() error                           { return nil }
