package cache

import (
	"bytes"
	"container/heap"
	"container/list"
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type entry struct {
	key       string
	data      []byte
	expiresAt time.Time
	// version identifies the latest write of key. Expiry records carry the
	// version they were created for, so an overwritten key's old record is
	// recognised and ignored instead of deleting the fresh entry.
	version uint64
	// elem is the entry's node in the evictable LRU list. It is nil for locks
	// and counters (SetNX/Increment/IncrementBelow): those are never evicted
	// while live, because evicting them would silently release a lock, reset a
	// lockout, or admit a request past its rate limit.
	elem *list.Element
}

func (e *entry) expired() bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return time.Now().After(e.expiresAt)
}

// expiry is one scheduled eviction in the expiryQueue.
type expiry struct {
	key     string
	at      time.Time
	version uint64
}

// expiryQueue is a min-heap by at, so expired entries are reclaimed without
// scanning the whole cache.
type expiryQueue []expiry

func (q expiryQueue) Len() int           { return len(q) }
func (q expiryQueue) Less(i, j int) bool { return q[i].at.Before(q[j].at) }
func (q expiryQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }

func (q *expiryQueue) Push(x any) {
	*q = append(*q, x.(expiry))
}

func (q *expiryQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	*q = old[:n-1]
	return item
}

// MemoryCache is a bounded process-local cache. The LRU list orders evictable
// cached values by recent use and the expiry heap reclaims expired entries
// first, so a client cannot grow the heap without bound. Locks and counters
// are stored outside the LRU and are only removed by Delete/CompareAndDelete or
// expiry; when they fill the cache, a new write fails with ErrFull.
type MemoryCache struct {
	mu          sync.Mutex
	items       map[string]*entry
	lru         *list.List
	expirations expiryQueue
	maxEntries  int
	nextVersion uint64
	done        chan struct{}
	closeOnce   sync.Once
}

// NewMemoryCache builds a cache with DefaultMemoryMaxEntries.
func NewMemoryCache() *MemoryCache {
	return NewMemoryCacheWithLimit(config.DefaultMemoryMaxEntries)
}

// NewMemoryCacheWithLimit builds a cache bounded to maxEntries. A non-positive
// limit falls back to DefaultMemoryMaxEntries; config validation rejects the
// value on the application path.
func NewMemoryCacheWithLimit(maxEntries int) *MemoryCache {
	if maxEntries <= 0 {
		maxEntries = config.DefaultMemoryMaxEntries
	}
	mc := &MemoryCache{
		items:      make(map[string]*entry),
		lru:        list.New(),
		maxEntries: maxEntries,
		done:       make(chan struct{}),
	}
	heap.Init(&mc.expirations)
	go mc.cleanup()
	return mc
}

func (m *MemoryCache) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.items[key]
	if !ok {
		return nil, nil
	}
	if e.expired() {
		m.removeLocked(e)
		return nil, nil
	}
	m.touchLocked(e)
	// Match the Redis driver: callers receive their own copy and cannot mutate
	// the cached value (or a value shared with another request).
	return bytes.Clone(e.data), nil
}

func (m *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if _, exists := m.items[key]; !exists && !m.makeRoomLocked(now) {
		return ErrFull
	}
	m.storeLocked(key, value, expiryAt(now, ttl), true)
	return nil
}

func (m *MemoryCache) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.items[key]; ok {
		if !e.expired() {
			return false, nil
		}
		m.removeLocked(e)
	}

	now := time.Now()
	if !m.makeRoomLocked(now) {
		return false, ErrFull
	}
	m.storeLocked(key, value, expiryAt(now, ttl), false)
	return true, nil
}

func (m *MemoryCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.items[key]; ok {
		m.removeLocked(e)
	}
	return nil
}

// CompareAndDelete deletes key only when its current value exactly matches
// value. It returns false when the key is missing, expired, or owned by someone
// else.
func (m *MemoryCache) CompareAndDelete(_ context.Context, key string, value []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.items[key]
	if !ok {
		return false, nil
	}
	if e.expired() {
		m.removeLocked(e)
		return false, nil
	}
	if !bytes.Equal(e.data, value) {
		return false, nil
	}
	m.removeLocked(e)
	return true, nil
}

func (m *MemoryCache) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.items[key]
	if !ok {
		return false, nil
	}
	if e.expired() {
		m.removeLocked(e)
		return false, nil
	}
	m.touchLocked(e)
	return true, nil
}

func (m *MemoryCache) Increment(_ context.Context, key string, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, ok, err := m.incrementLocked(key, ttl, math.MaxInt64)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("cache: increment overflow at %q", key)
	}
	return value, nil
}

// IncrementBelow increments only while the resulting value stays at or below
// maxValue, leaving the counter untouched (and ok false) once the cap is reached.
func (m *MemoryCache) IncrementBelow(_ context.Context, key string, ttl time.Duration, maxValue int64) (value int64, ok bool, err error) {
	if maxValue < 1 {
		m.mu.Lock()
		defer m.mu.Unlock()
		value, _, err = m.currentLocked(key)
		return value, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.incrementLocked(key, ttl, maxValue)
}

// incrementLocked applies the shared Increment/IncrementBelow semantics. It
// returns ok=false without writing when the counter is already at maxValue. The
// caller holds m.mu.
func (m *MemoryCache) incrementLocked(key string, ttl time.Duration, maxValue int64) (value int64, ok bool, err error) {
	if e, exists := m.items[key]; exists {
		if !e.expired() {
			n, parseErr := strconv.ParseInt(string(e.data), 10, 64)
			if parseErr != nil {
				// Match Redis INCR: a non-integer value is not a counter.
				return 0, false, fmt.Errorf("cache: increment non-integer value at %q", key)
			}
			// A counter is never evictable while live, even if the key was
			// first written as a cached value.
			m.markNonEvictableLocked(e)
			if n >= maxValue {
				return n, false, nil
			}
			n++
			// Preserve the original expiry and version: a new expiry record
			// would make the existing heap entry stale.
			e.data = []byte(strconv.FormatInt(n, 10))
			return n, true, nil
		}
		m.removeLocked(e)
	}

	if maxValue < 1 {
		return 0, false, nil
	}
	now := time.Now()
	if !m.makeRoomLocked(now) {
		return 0, false, ErrFull
	}
	m.storeLocked(key, []byte("1"), expiryAt(now, ttl), false)
	return 1, true, nil
}

// currentLocked returns a live counter's value without writing it.
func (m *MemoryCache) currentLocked(key string) (value int64, ok bool, err error) {
	e, exists := m.items[key]
	if !exists {
		return 0, false, nil
	}
	if e.expired() {
		m.removeLocked(e)
		return 0, false, nil
	}
	n, err := strconv.ParseInt(string(e.data), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("cache: increment non-integer value at %q", key)
	}
	m.touchLocked(e)
	return n, true, nil
}

// touchLocked marks an evictable entry as recently used. Locks and counters
// have no LRU node and need no ordering. The caller holds m.mu.
func (m *MemoryCache) touchLocked(e *entry) {
	if e.elem != nil {
		m.lru.MoveToFront(e.elem)
	}
}

// markNonEvictableLocked removes an entry from the LRU, so it is never chosen
// for capacity eviction while live. The caller holds m.mu.
func (m *MemoryCache) markNonEvictableLocked(e *entry) {
	if e.elem != nil {
		m.lru.Remove(e.elem)
		e.elem = nil
	}
}

// storeLocked writes key and records its expiry, placing evictable entries at
// the front of the LRU. The caller holds m.mu.
func (m *MemoryCache) storeLocked(key string, value []byte, expiresAt time.Time, evictable bool) {
	m.nextVersion++
	e, ok := m.items[key]
	if !ok {
		e = &entry{key: key}
		m.items[key] = e
	}
	e.data = bytes.Clone(value)
	e.expiresAt = expiresAt
	e.version = m.nextVersion
	if evictable {
		if e.elem == nil {
			e.elem = m.lru.PushFront(e)
		} else {
			m.lru.MoveToFront(e.elem)
		}
	} else {
		m.markNonEvictableLocked(e)
	}
	if !expiresAt.IsZero() {
		heap.Push(&m.expirations, expiry{key: key, at: expiresAt, version: m.nextVersion})
	}
	m.compactExpirationsLocked()
}

// makeRoomLocked evicts expired entries first and then the least recently used
// cached value until one slot is free. It reports false when every live entry
// is non-evictable (a lock or counter): the caller must then fail the write
// with ErrFull instead of dropping live state. The caller holds m.mu.
func (m *MemoryCache) makeRoomLocked(now time.Time) bool {
	for len(m.items) >= m.maxEntries {
		if m.dropExpiredLocked(now) {
			continue
		}
		el := m.lru.Back()
		if el == nil {
			return false
		}
		m.removeLocked(el.Value.(*entry))
	}
	return true
}

// dropExpiredLocked removes every entry whose expiry record is due and reports
// whether it removed any. Stale records (an overwritten key) are discarded
// without touching the current entry.
func (m *MemoryCache) dropExpiredLocked(now time.Time) bool {
	removed := false
	for len(m.expirations) > 0 {
		next := m.expirations[0]
		if next.at.After(now) {
			break
		}
		heap.Pop(&m.expirations)
		e, ok := m.items[next.key]
		if !ok {
			continue
		}
		if e.version != next.version {
			continue
		}
		m.removeLocked(e)
		removed = true
	}
	return removed
}

func (m *MemoryCache) removeLocked(e *entry) {
	delete(m.items, e.key)
	if e.elem != nil {
		m.lru.Remove(e.elem)
		e.elem = nil
	}
}

// compactExpirationsLocked bounds the expiry heap when keys are overwritten
// many times: rebuilding is O(n) but only after the heap has grown to twice
// the live entry count, so its amortised cost stays O(1) per write.
func (m *MemoryCache) compactExpirationsLocked() {
	if len(m.expirations) <= 2*len(m.items)+64 {
		return
	}
	compact := make(expiryQueue, 0, len(m.items))
	for _, e := range m.items {
		if !e.expiresAt.IsZero() {
			compact = append(compact, expiry{key: e.key, at: e.expiresAt, version: e.version})
		}
	}
	heap.Init(&compact)
	m.expirations = compact
}

func expiryAt(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

func (m *MemoryCache) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			m.dropExpiredLocked(time.Now())
			m.compactExpirationsLocked()
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
