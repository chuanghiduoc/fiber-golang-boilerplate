package cache

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
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
		return NewMemoryCache(), nil
	}
}
