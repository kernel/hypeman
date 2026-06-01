package uffdpager

import (
	"container/list"
	"sync"
)

type pageKey struct {
	cacheKey string
	offset   int64
	size     int
}

type cacheEntry struct {
	key  pageKey
	data []byte
}

type PageCache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	items    map[pageKey]*list.Element
	lru      *list.List
	hits     int64
	misses   int64
}

func NewPageCache(maxBytes int64) *PageCache {
	return &PageCache{
		maxBytes: normalizeCacheMaxBytes(maxBytes),
		items:    make(map[pageKey]*list.Element),
		lru:      list.New(),
	}
}

func (c *PageCache) Get(cacheKey string, offset int64, size int) ([]byte, bool) {
	key := pageKey{cacheKey: cacheKey, offset: offset, size: size}

	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	c.lru.MoveToFront(elem)
	entry := elem.Value.(*cacheEntry)
	data := append([]byte(nil), entry.data...)
	return data, true
}

func (c *PageCache) Add(cacheKey string, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}
	key := pageKey{cacheKey: cacheKey, offset: offset, size: len(data)}
	value := append([]byte(nil), data...)

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*cacheEntry)
		c.bytes -= int64(len(entry.data))
		entry.data = value
		c.bytes += int64(len(entry.data))
		c.lru.MoveToFront(elem)
		c.evictLocked()
		return
	}

	elem := c.lru.PushFront(&cacheEntry{key: key, data: value})
	c.items[key] = elem
	c.bytes += int64(len(value))
	c.evictLocked()
}

func (c *PageCache) SnapshotStats() (bytes, maxBytes int64, items int, hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes, c.maxBytes, len(c.items), c.hits, c.misses
}

func (c *PageCache) evictLocked() {
	for c.bytes > c.maxBytes && c.lru.Len() > 0 {
		elem := c.lru.Back()
		entry := elem.Value.(*cacheEntry)
		delete(c.items, entry.key)
		c.bytes -= int64(len(entry.data))
		c.lru.Remove(elem)
	}
}
