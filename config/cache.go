package config

import (
	"fmt"
	"time"
)

// DefaultMemoryMaxEntries bounds the process-local cache: every stored entry
// keeps its full value in memory (idempotency responses can be large), so an
// unbounded map lets clients grow the heap until the process is OOM-killed.
const DefaultMemoryMaxEntries = 100000

type CacheConfig struct {
	Driver   string `env:"CACHE_DRIVER" envDefault:"memory"`
	RedisURL string `env:"REDIS_URL"`
	// MemoryMaxEntries bounds the memory driver. Expired entries are dropped
	// first, then the least recently used ones. Validation requires > 0.
	MemoryMaxEntries int `env:"CACHE_MEMORY_MAX_ENTRIES" envDefault:"100000"`
	// AllowMemoryInProduction acknowledges a single-replica production
	// deployment. The memory driver keeps rate limits, idempotency, login
	// lockout, and session invalidation per process, so it is rejected in
	// production unless this is explicitly true.
	AllowMemoryInProduction bool `env:"CACHE_ALLOW_MEMORY_IN_PRODUCTION" envDefault:"false"`
	// Redis client bounds. go-redis defaults (5s dial, 3s read, 3 retries)
	// would turn a blackholed Redis into multi-second stalls on every request
	// even though rate limiting and idempotency fail open, so these are short.
	// They override timeout/retry parameters in REDIS_URL.
	RedisDialTimeout  time.Duration `env:"REDIS_DIAL_TIMEOUT" envDefault:"2s"`
	RedisReadTimeout  time.Duration `env:"REDIS_READ_TIMEOUT" envDefault:"500ms"`
	RedisWriteTimeout time.Duration `env:"REDIS_WRITE_TIMEOUT" envDefault:"500ms"`
	RedisPoolTimeout  time.Duration `env:"REDIS_POOL_TIMEOUT" envDefault:"1s"`
	// RedisMaxRetries is the number of retries after the first attempt; 0
	// disables retries.
	RedisMaxRetries int `env:"REDIS_MAX_RETRIES" envDefault:"1"`
}

// Redis client defaults, used when a CacheConfig literal leaves a timeout at
// zero (tests and tools); Load always fills the env defaults.
const (
	DefaultRedisDialTimeout  = 2 * time.Second
	DefaultRedisReadTimeout  = 500 * time.Millisecond
	DefaultRedisWriteTimeout = 500 * time.Millisecond
	DefaultRedisPoolTimeout  = time.Second
	maxRedisRetries          = 10
)

func durationOrDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// EffectiveRedisDialTimeout returns the Redis connect timeout, applying the
// package default to a zero value built outside env parsing.
func (c CacheConfig) EffectiveRedisDialTimeout() time.Duration {
	return durationOrDefault(c.RedisDialTimeout, DefaultRedisDialTimeout)
}

// EffectiveRedisReadTimeout returns the Redis per-command read timeout.
func (c CacheConfig) EffectiveRedisReadTimeout() time.Duration {
	return durationOrDefault(c.RedisReadTimeout, DefaultRedisReadTimeout)
}

// EffectiveRedisWriteTimeout returns the Redis per-command write timeout.
func (c CacheConfig) EffectiveRedisWriteTimeout() time.Duration {
	return durationOrDefault(c.RedisWriteTimeout, DefaultRedisWriteTimeout)
}

// EffectiveRedisPoolTimeout returns how long a command waits for a free
// connection.
func (c CacheConfig) EffectiveRedisPoolTimeout() time.Duration {
	return durationOrDefault(c.RedisPoolTimeout, DefaultRedisPoolTimeout)
}

// Validate checks the cache driver and Redis client bounds.
func (c CacheConfig) Validate() error {
	switch c.Driver {
	case "memory":
	case "redis":
		if c.RedisURL == "" {
			return fmt.Errorf("REDIS_URL is required when CACHE_DRIVER=redis")
		}
	default:
		return fmt.Errorf("CACHE_DRIVER must be one of: memory, redis (got %q)", c.Driver)
	}
	if c.MemoryMaxEntries <= 0 {
		return fmt.Errorf("CACHE_MEMORY_MAX_ENTRIES must be greater than 0")
	}
	if c.RedisDialTimeout < 0 || c.RedisReadTimeout < 0 || c.RedisWriteTimeout < 0 || c.RedisPoolTimeout < 0 {
		return fmt.Errorf("REDIS_*_TIMEOUT values must not be negative")
	}
	if c.RedisMaxRetries < 0 || c.RedisMaxRetries > maxRedisRetries {
		return fmt.Errorf("REDIS_MAX_RETRIES must be between 0 and %d", maxRedisRetries)
	}
	return nil
}

// validateProduction rejects the per-process memory driver unless a
// single-replica deployment explicitly acknowledges it.
func (c CacheConfig) validateProduction() error {
	if c.Driver == "memory" && !c.AllowMemoryInProduction {
		return fmt.Errorf("CACHE_DRIVER=memory is not shared across replicas; use redis in production or set CACHE_ALLOW_MEMORY_IN_PRODUCTION=true for a single-replica deployment")
	}
	return nil
}
