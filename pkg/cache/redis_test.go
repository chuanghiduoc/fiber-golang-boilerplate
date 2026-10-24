package cache

import (
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func TestRedisOptionsApplyConfiguredBounds(t *testing.T) {
	opts, err := redisOptions(config.CacheConfig{
		RedisURL:            "redis://localhost:6379/0?dial_timeout=30s&read_timeout=30s&max_retries=5",
		RedisDialTimeoutMS:  1500,
		RedisReadTimeoutMS:  400,
		RedisWriteTimeoutMS: 300,
		RedisPoolTimeoutMS:  700,
		RedisMaxRetries:     2,
	})
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	if opts.DialTimeout != 1500*time.Millisecond || opts.ReadTimeout != 400*time.Millisecond ||
		opts.WriteTimeout != 300*time.Millisecond || opts.PoolTimeout != 700*time.Millisecond || opts.MaxRetries != 2 {
		t.Fatalf("configured bounds not applied: dial=%v read=%v write=%v pool=%v retries=%d",
			opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout, opts.PoolTimeout, opts.MaxRetries)
	}
}

func TestRedisOptionsDefaultsAndZeroRetries(t *testing.T) {
	opts, err := redisOptions(config.CacheConfig{RedisURL: "redis://localhost:6379/0"})
	if err != nil {
		t.Fatalf("redisOptions: %v", err)
	}
	if opts.DialTimeout != config.DefaultRedisDialTimeout || opts.ReadTimeout != config.DefaultRedisReadTimeout ||
		opts.WriteTimeout != config.DefaultRedisWriteTimeout || opts.PoolTimeout != config.DefaultRedisPoolTimeout {
		t.Fatalf("zero config must use the package defaults, got dial=%v read=%v write=%v pool=%v",
			opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout, opts.PoolTimeout)
	}
	// 0 means "no retries"; go-redis would read 0 as its default of 3.
	if opts.MaxRetries != -1 {
		t.Fatalf("MaxRetries = %d, want -1 (disabled) for REDIS_MAX_RETRIES=0", opts.MaxRetries)
	}
}

func TestRedisOptionsRejectsInvalidURL(t *testing.T) {
	if _, err := redisOptions(config.CacheConfig{RedisURL: "http://not-redis"}); err == nil {
		t.Fatal("redisOptions should reject a non-redis URL")
	}
}
