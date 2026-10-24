package cache

import (
	"context"
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
	opts.DialTimeout = cfg.RedisDialTimeout()
	opts.ReadTimeout = cfg.RedisReadTimeout()
	opts.WriteTimeout = cfg.RedisWriteTimeout()
	opts.PoolTimeout = cfg.RedisPoolTimeout()
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

// incrExpireScript increments a key and, on first creation, sets its expiry —
// atomically, so a crash between INCR and EXPIRE cannot leave a key without a
// TTL (which would lock the account out permanently).
var incrExpireScript = redis.NewScript(`
local v = redis.call('INCR', KEYS[1])
if v == 1 and tonumber(ARGV[1]) > 0 then
	redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return v
`)

func (r *RedisCache) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return incrExpireScript.Run(ctx, r.client, []string{key}, ttl.Milliseconds()).Int64()
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
