//go:build integration

package cache

import (
	"context"
	"os"
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
