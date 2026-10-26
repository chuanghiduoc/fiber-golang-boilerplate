//go:build integration

package cache

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

const redisImage = "redis:8-alpine"

// redisURL returns a Redis URL. Set TEST_REDIS_URL to reuse a running server;
// otherwise a disposable container is started.
func redisURL(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("TEST_REDIS_URL"); url != "" {
		return url
	}

	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        redisImage,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	endpoint, err := container.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		t.Fatalf("redis endpoint: %v", err)
	}
	return "redis://" + endpoint + "/0"
}

func TestRedisCacheSetNX(t *testing.T) {
	rc, err := NewRedisCache(config.CacheConfig{RedisURL: redisURL(t)})
	if err != nil {
		t.Fatalf("NewRedisCache: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()

	if stored, err := rc.SetNX(ctx, "it:setnx", []byte("owner-1"), time.Minute); err != nil || !stored {
		t.Fatalf("SetNX(absent) = %v, %v; want true, nil", stored, err)
	}
	if stored, err := rc.SetNX(ctx, "it:setnx", []byte("owner-2"), time.Minute); err != nil || stored {
		t.Fatalf("SetNX(held) = %v, %v; want false, nil", stored, err)
	}
	if got, _ := rc.Get(ctx, "it:setnx"); string(got) != "owner-1" {
		t.Fatalf("value = %q, want owner-1", got)
	}
}

func TestRedisCacheIncrementBelow(t *testing.T) {
	rc, err := NewRedisCache(config.CacheConfig{RedisURL: redisURL(t)})
	if err != nil {
		t.Fatalf("NewRedisCache: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()
	key := "it:increment-below"

	for want := int64(1); want <= 2; want++ {
		got, ok, err := rc.IncrementBelow(ctx, key, time.Minute, 2)
		if err != nil {
			t.Fatalf("IncrementBelow: %v", err)
		}
		stored, _ := rc.Get(ctx, key)
		if !ok || got != want || string(stored) != strconv.FormatInt(want, 10) {
			t.Fatalf("IncrementBelow = %d, %v, stored %q; want %d, true", got, ok, stored, want)
		}
	}
	got, ok, err := rc.IncrementBelow(ctx, key, time.Minute, 2)
	if err != nil {
		t.Fatalf("IncrementBelow at cap: %v", err)
	}
	if ok || got != 2 {
		t.Fatalf("IncrementBelow at cap = %d, %v; want 2, false", got, ok)
	}

	// A rejected first increment must not create the key.
	absent := "it:increment-below-absent"
	got, ok, err = rc.IncrementBelow(ctx, absent, time.Minute, 0)
	if err != nil {
		t.Fatalf("IncrementBelow(absent): %v", err)
	}
	if ok || got != 0 {
		t.Fatalf("IncrementBelow(absent, max 0) = %d, %v; want 0, false", got, ok)
	}
	if exists, err := rc.Exists(ctx, absent); err != nil || exists {
		t.Fatalf("Exists(absent) = %v, %v; want false, nil", exists, err)
	}
}

// TestRedisCacheIncrementBelowKeepsTTL is the regression test for the
// SET-without-KEEPTTL script bug: only the first increment used to leave a
// TTL, so a rate-limit key hit a second time lived forever. PTTL must stay
// positive after every admission.
func TestRedisCacheIncrementBelowKeepsTTL(t *testing.T) {
	rc, err := NewRedisCache(config.CacheConfig{RedisURL: redisURL(t)})
	if err != nil {
		t.Fatalf("NewRedisCache: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()
	key := "it:increment-below-ttl"
	const ttl = time.Minute
	if err := rc.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(%s): %v", key, err)
	}

	for want := int64(1); want <= 5; want++ {
		got, ok, err := rc.IncrementBelow(ctx, key, ttl, 10)
		if err != nil || !ok || got != want {
			t.Fatalf("IncrementBelow #%d = %d, %v, %v; want %d, true, nil", want, got, ok, err, want)
		}
		pttl, err := rc.client.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL after increment #%d: %v", want, err)
		}
		if pttl <= 0 {
			t.Fatalf("PTTL after increment #%d = %v, want > 0 (key must not become immortal)", want, pttl)
		}
		if pttl > ttl {
			t.Fatalf("PTTL after increment #%d = %v, want <= %v", want, pttl, ttl)
		}
	}
}

// TestRedisCacheIncrementRepairsImmortalKey covers the audit of Increment:
// INCR preserves a TTL but cannot add one, so a key that already exists
// without an expiry (for example one written by the old IncrementBelow script)
// must gain the requested TTL instead of staying immortal.
func TestRedisCacheIncrementRepairsImmortalKey(t *testing.T) {
	rc, err := NewRedisCache(config.CacheConfig{RedisURL: redisURL(t)})
	if err != nil {
		t.Fatalf("NewRedisCache: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	ctx := context.Background()

	for _, key := range []string{"it:increment-immortal", "it:increment-below-immortal"} {
		if err := rc.client.Set(ctx, key, 5, 0).Err(); err != nil {
			t.Fatalf("seed %s without TTL: %v", key, err)
		}
		var got int64
		if key == "it:increment-immortal" {
			got, err = rc.Increment(ctx, key, time.Minute)
		} else {
			got, _, err = rc.IncrementBelow(ctx, key, time.Minute, 10)
		}
		if err != nil || got != 6 {
			t.Fatalf("%s increment = %d, %v; want 6, nil", key, got, err)
		}
		pttl, err := rc.client.PTTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("PTTL(%s): %v", key, err)
		}
		if pttl <= 0 {
			t.Fatalf("PTTL(%s) = %v, want the requested TTL to be applied", key, pttl)
		}
	}
}

// TestRedisCacheHonoursContextDeadline stalls the server with CLIENT PAUSE and
// proves an operation returns at its context deadline, not the (deliberately
// long) socket read timeout.
func TestRedisCacheHonoursContextDeadline(t *testing.T) {
	url := redisURL(t)
	rc, err := NewRedisCache(config.CacheConfig{RedisURL: url, RedisReadTimeoutMS: 5000, RedisWriteTimeoutMS: 5000})
	if err != nil {
		t.Fatalf("NewRedisCache: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	admin := redis.NewClient(opts)
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Do(context.Background(), "CLIENT", "PAUSE", "2000", "ALL").Err(); err != nil {
		t.Fatalf("CLIENT PAUSE: %v", err)
	}
	t.Cleanup(func() { _ = admin.Do(context.Background(), "CLIENT", "UNPAUSE").Err() })

	const bound = 250 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	start := time.Now()
	_, err = rc.Get(ctx, "it:paused")
	elapsed := time.Since(start)

	// go-redis applies the context deadline to the socket, so the error is a
	// net timeout rather than context.DeadlineExceeded; the bound is what
	// matters.
	if err == nil {
		t.Fatal("Get on a paused server succeeded, want a timeout")
	}
	t.Logf("Get on paused server returned after %v: %v", elapsed, err)
	if elapsed > bound+250*time.Millisecond {
		t.Fatalf("Get took %v on a paused server, want about %v (context deadline)", elapsed, bound)
	}
}
