package uffdpager

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

type pageKey struct {
	cacheKey string
	offset   int64
	size     int
}

type cacheEntry struct {
	key  pageKey
	data []byte
	// ref is the CLOCK reference bit: set on access, cleared when the eviction
	// sweep passes over the entry. Atomic because concurrent Borrows set it
	// while holding only the shard read lock.
	ref atomic.Bool
}

type PageCache struct {
	shards []*pageCacheShard
}

type pageCacheShard struct {
	mu       sync.RWMutex
	maxBytes int64
	bytes    int64
	items    map[pageKey]*list.Element
	ring     *list.List // CLOCK sweep order; evictLocked walks from the back

	hits   atomic.Int64
	misses atomic.Int64

	lookupNanos    atomic.Int64
	lookupMaxNanos atomic.Int64
	addNanos       atomic.Int64
	addMaxNanos    atomic.Int64
}

func NewPageCache(maxBytes int64) *PageCache {
	maxBytes = normalizeCacheMaxBytes(maxBytes)
	shardCount := pageCacheShardCount(maxBytes)
	shards := make([]*pageCacheShard, shardCount)
	shardMax := maxBytes / int64(shardCount)
	if shardMax <= 0 {
		shardMax = maxBytes
	}
	for i := range shards {
		shards[i] = &pageCacheShard{
			maxBytes: shardMax,
			items:    make(map[pageKey]*list.Element),
			ring:     list.New(),
		}
	}
	return &PageCache{
		shards: shards,
	}
}

func (c *PageCache) Get(cacheKey string, offset int64, size int) ([]byte, bool) {
	data, ok := c.lookup(pageKey{cacheKey: cacheKey, offset: offset, size: size})
	if !ok {
		return nil, false
	}
	return append([]byte(nil), data...), true
}

// Borrow returns the immutable cached page without copying. It marks the page
// referenced so the next CLOCK eviction sweep spares it.
func (c *PageCache) Borrow(cacheKey string, offset int64, size int) ([]byte, bool) {
	return c.lookup(pageKey{cacheKey: cacheKey, offset: offset, size: size})
}

// lookup marks a hit's reference bit under the read lock. Unlike LRU it does not
// reorder the ring, so concurrent faults on hot pages don't serialize behind a
// write lock.
func (c *PageCache) lookup(key pageKey) ([]byte, bool) {
	start := time.Now()
	shard := c.shardFor(key)

	shard.mu.RLock()
	elem, ok := shard.items[key]
	if !ok {
		shard.mu.RUnlock()
		shard.misses.Add(1)
		shard.recordLookupDuration(time.Since(start))
		return nil, false
	}
	entry := elem.Value.(*cacheEntry)
	entry.ref.Store(true)
	data := entry.data
	shard.mu.RUnlock()
	shard.hits.Add(1)
	shard.recordLookupDuration(time.Since(start))
	return data, true
}

func (c *PageCache) Add(cacheKey string, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}
	start := time.Now()
	key := pageKey{cacheKey: cacheKey, offset: offset, size: len(data)}
	value := append([]byte(nil), data...)
	shard := c.shardFor(key)

	shard.mu.Lock()
	defer shard.mu.Unlock()
	defer func() {
		shard.recordAddDuration(time.Since(start))
	}()

	if elem, ok := shard.items[key]; ok {
		entry := elem.Value.(*cacheEntry)
		shard.bytes -= int64(len(entry.data))
		entry.data = value
		shard.bytes += int64(len(entry.data))
		entry.ref.Store(true)
		shard.evictLocked()
		return
	}

	elem := shard.ring.PushFront(&cacheEntry{key: key, data: value})
	shard.items[key] = elem
	shard.bytes += int64(len(value))
	shard.evictLocked()
}

func (c *PageCache) SnapshotStats() (bytes, maxBytes int64, items int, hits, misses int64) {
	for _, shard := range c.shards {
		shard.mu.Lock()
		bytes += shard.bytes
		maxBytes += shard.maxBytes
		items += len(shard.items)
		shard.mu.Unlock()
		hits += shard.hits.Load()
		misses += shard.misses.Load()
	}
	return bytes, maxBytes, items, hits, misses
}

func (c *PageCache) SnapshotTimingStats() (shards int, lookupNanos, lookupMaxNanos, addNanos, addMaxNanos int64) {
	for _, shard := range c.shards {
		shards++
		lookupNanos += shard.lookupNanos.Load()
		if max := shard.lookupMaxNanos.Load(); max > lookupMaxNanos {
			lookupMaxNanos = max
		}
		addNanos += shard.addNanos.Load()
		if max := shard.addMaxNanos.Load(); max > addMaxNanos {
			addMaxNanos = max
		}
	}
	return shards, lookupNanos, lookupMaxNanos, addNanos, addMaxNanos
}

func (c *PageCache) shardFor(key pageKey) *pageCacheShard {
	if len(c.shards) == 1 {
		return c.shards[0]
	}
	return c.shards[int(hashPageKey(key)%uint64(len(c.shards)))]
}

// evictLocked is the CLOCK sweep: walk from the back, sparing referenced entries
// once (clearing their bit) and evicting the first unreferenced one, until the
// shard is back under budget.
func (c *pageCacheShard) evictLocked() {
	for c.bytes > c.maxBytes && c.ring.Len() > 0 {
		elem := c.ring.Back()
		entry := elem.Value.(*cacheEntry)
		if entry.ref.Load() {
			entry.ref.Store(false)
			c.ring.MoveToFront(elem)
			continue
		}
		delete(c.items, entry.key)
		c.bytes -= int64(len(entry.data))
		c.ring.Remove(elem)
	}
}

func (c *pageCacheShard) recordLookupDuration(duration time.Duration) {
	nanos := duration.Nanoseconds()
	c.lookupNanos.Add(nanos)
	atomicMaxCacheInt64(&c.lookupMaxNanos, nanos)
}

func (c *pageCacheShard) recordAddDuration(duration time.Duration) {
	nanos := duration.Nanoseconds()
	c.addNanos.Add(nanos)
	atomicMaxCacheInt64(&c.addMaxNanos, nanos)
}

func pageCacheShardCount(maxBytes int64) int {
	const targetShardBytes = int64(64 << 20)
	count := int(maxBytes / targetShardBytes)
	if count < 1 {
		return 1
	}
	if count > 64 {
		return 64
	}
	return count
}

func hashPageKey(key pageKey) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	hash := uint64(offset64)
	for i := 0; i < len(key.cacheKey); i++ {
		hash ^= uint64(key.cacheKey[i])
		hash *= prime64
	}
	hash ^= uint64(key.offset)
	hash *= prime64
	hash ^= uint64(key.offset >> 32)
	hash *= prime64
	hash ^= uint64(key.size)
	hash *= prime64
	return mixPageHash(hash)
}

func mixPageHash(hash uint64) uint64 {
	hash ^= hash >> 33
	hash *= 0xff51afd7ed558ccd
	hash ^= hash >> 33
	hash *= 0xc4ceb9fe1a85ec53
	hash ^= hash >> 33
	return hash
}

func atomicMaxCacheInt64(target *atomic.Int64, candidate int64) {
	for {
		current := target.Load()
		if candidate <= current || target.CompareAndSwap(current, candidate) {
			return
		}
	}
}
