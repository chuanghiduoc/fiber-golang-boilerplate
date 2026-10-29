package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"

	"github.com/redis/go-redis/v9"
)

type RedisCache struct {
	client *redis.Client
}

func NewRedisCache(cfg config.CacheConfig) (*RedisCache, error) {
	opts, err := redisOptions(cfg)
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		// Release the pool and its background goroutines before reporting the failure.
		_ = client.Close()
		return nil, err
	}

	return &RedisCache{client: client}, nil
}

// redisOptions parses REDIS_URL and applies the configured client bounds,
// overriding any timeout/retry parameters in the URL. Short bounds make an
// unreachable Redis fail fast, so fail-open callers (rate limiting,
// idempotency) do not stall requests for go-redis's multi-second defaults.
func redisOptions(cfg config.CacheConfig) (*redis.Options, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	opts.DialTimeout = cfg.EffectiveRedisDialTimeout()
	opts.ReadTimeout = cfg.EffectiveRedisReadTimeout()
	opts.WriteTimeout = cfg.EffectiveRedisWriteTimeout()
	opts.PoolTimeout = cfg.EffectiveRedisPoolTimeout()
	// Without this go-redis ignores context deadlines on socket I/O and waits
	// the full read/write timeout, so callers' tighter per-call bounds (rate
	// limit, idempotency) would have no effect against a stalled server.
	opts.ContextTimeoutEnabled = true
	// go-redis treats 0 as "use the default of 3 retries" and -1 as none.
	opts.MaxRetries = cfg.RedisMaxRetries
	if opts.MaxRetries == 0 {
		opts.MaxRetries = -1
	}
	return opts, nil
}

func (r *RedisCache) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return val, nil
}

func (r *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

// SetNX maps to SET key value NX PX ttl, a single atomic command.
func (r *RedisCache) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return r.client.SetNX(ctx, key, value, ttl).Result()
}

func (r *RedisCache) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

func (r *RedisCache) Exists(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// incrExpireScript increments a key and applies the expiry atomically, so a
// crash between INCR and EXPIRE cannot leave a key without a TTL (which would
// lock the account out permanently). INCR preserves an existing TTL, but a key
// that already exists without one (for example a counter written by an older
// client) gets the requested TTL too.
var incrExpireScript = redis.NewScript(`
local v = redis.call('INCR', KEYS[1])
if tonumber(ARGV[1]) > 0 and (v == 1 or redis.call('PTTL', KEYS[1]) < 0) then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return v
`)

func (r *RedisCache) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return incrExpireScript.Run(ctx, r.client, []string{key}, ttl.Milliseconds()).Int64()
}

// incrBelowExpireScript increments only while the result stays at or below
// ARGV[2], returning {value, allowed}. Rejected increments do not create a key
// or extend its TTL, so a throttled client's retries cannot extend its block.
// SET keeps the existing TTL (KEEPTTL) and a key that is new or has none gets
// the requested one; a plain SET would drop the expiry on every increment after
// the first and leave the key alive forever.
var incrBelowExpireScript = redis.NewScript(`
local raw = redis.call('GET', KEYS[1])
local v = 0
if raw then
	v = tonumber(raw)
	if not v then
		return redis.error_reply('cache: increment non-integer value')
	end
end
local maxValue = tonumber(ARGV[2])
if v >= maxValue then
	return {v, 0}
end
v = v + 1
redis.call('SET', KEYS[1], v, 'KEEPTTL')
if tonumber(ARGV[1]) > 0 and redis.call('PTTL', KEYS[1]) < 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return {v, 1}
`)

func (r *RedisCache) IncrementBelow(ctx context.Context, key string, ttl time.Duration, maxValue int64) (value int64, ok bool, err error) {
	values, err := incrBelowExpireScript.Run(ctx, r.client, []string{key}, ttl.Milliseconds(), maxValue).Int64Slice()
	if err != nil {
		return 0, false, err
	}
	if len(values) != 2 {
		return 0, false, fmt.Errorf("cache: increment-below script returned %d values, want 2", len(values))
	}
	return values[0], values[1] == 1, nil
}

// compareAndDeleteScript deletes the key only when its value still matches the
// owner token. Keeping the comparison in Redis makes release atomic, so an
// expired-and-reacquired lock cannot be deleted by its previous owner.
var compareAndDeleteScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

func (r *RedisCache) CompareAndDelete(ctx context.Context, key string, value []byte) (bool, error) {
	deleted, err := compareAndDeleteScript.Run(ctx, r.client, []string{key}, value).Int64()
	if err != nil {
		return false, err
	}
	return deleted == 1, nil
}

func (r *RedisCache) Close() error {
	return r.client.Close()
}

func (r *RedisCache) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}
