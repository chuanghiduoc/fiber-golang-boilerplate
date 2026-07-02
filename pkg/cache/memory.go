package cache

import (
	"context"
	"strconv"
	"sync"
	"time"
)

type entry struct {
	data      []byte
	expiresAt time.Time
}

func (e entry) expired() bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return time.Now().After(e.expiresAt)
}

type MemoryCache struct {
	mu        sync.RWMutex
	items     map[string]entry
	done      chan struct{}
	closeOnce sync.Once
}

func NewMemoryCache() *MemoryCache {
	mc := &MemoryCache{items: make(map[string]entry), done: make(chan struct{})}
	go mc.cleanup()
	return mc
}

func (m *MemoryCache) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	e, ok := m.items[key]
	m.mu.RUnlock()

	if !ok || e.expired() {
		return nil, nil
	}
	return e.data, nil
}

func (m *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}

	m.items[key] = entry{data: value, expiresAt: expiresAt}
	return nil
}

func (m *MemoryCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.items, key)
	return nil
}

func (m *MemoryCache) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	e, ok := m.items[key]
	m.mu.RUnlock()

	if !ok || e.expired() {
		return false, nil
	}
	return true, nil
}

func (m *MemoryCache) Increment(_ context.Context, key string, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.items[key]; ok && !e.expired() {
		n, _ := strconv.ParseInt(string(e.data), 10, 64)
		n++
		// Preserve the original expiry so the lockout window is not extended by
		// each subsequent attempt.
		m.items[key] = entry{data: []byte(strconv.FormatInt(n, 10)), expiresAt: e.expiresAt}
		return n, nil
	}

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	m.items[key] = entry{data: []byte("1"), expiresAt: expiresAt}
	return 1, nil
}

func (m *MemoryCache) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			for k, e := range m.items {
				if e.expired() {
					delete(m.items, k)
				}
			}
			m.mu.Unlock()
		case <-m.done:
			return
		}
	}
}

func (m *MemoryCache) Close() error {
	m.closeOnce.Do(func() { close(m.done) })
	return nil
}

func (m *MemoryCache) Ping(_ context.Context) error {
	return nil
}
