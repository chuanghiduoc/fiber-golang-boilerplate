package middleware

import (
	"context"
	"log/slog"
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

// Cache failures fail open (skip the limit) rather than turning a cache blip
// into a 500 for every API request. Login lockout still fails closed in the
// user service.
func (s *cacheLimiterStorage) GetWithContext(ctx context.Context, key string) ([]byte, error) {
	value, err := s.cache.Get(ctx, key)
	if err != nil {
		slog.Warn("rate-limit cache read failed; allowing request", slog.Any("error", err))
		return nil, nil
	}
	return value, nil
}

func (s *cacheLimiterStorage) Get(key string) ([]byte, error) {
	return s.GetWithContext(context.Background(), key)
}

func (s *cacheLimiterStorage) SetWithContext(ctx context.Context, key string, val []byte, exp time.Duration) error {
	if err := s.cache.Set(ctx, key, val, exp); err != nil {
		slog.Warn("rate-limit cache write failed; allowing request", slog.Any("error", err))
	}
	return nil
}

func (s *cacheLimiterStorage) Set(key string, val []byte, exp time.Duration) error {
	return s.SetWithContext(context.Background(), key, val, exp)
}

func (s *cacheLimiterStorage) DeleteWithContext(ctx context.Context, key string) error {
	if err := s.cache.Delete(ctx, key); err != nil {
		slog.Warn("rate-limit cache delete failed", slog.Any("error", err))
	}
	return nil
}

func (s *cacheLimiterStorage) Delete(key string) error {
	return s.DeleteWithContext(context.Background(), key)
}

// Reset/Close are not used by the limiter at runtime. Scanning and deleting
// every application cache key would be dangerous, so they are no-ops.
func (s *cacheLimiterStorage) ResetWithContext(context.Context) error { return nil }
func (s *cacheLimiterStorage) Reset() error                           { return nil }
func (s *cacheLimiterStorage) Close() error                           { return nil }
