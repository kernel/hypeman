package cachebench

import (
	"container/list"
	"sync"
	"sync/atomic"
)

type clockEntry struct {
	key  pageKey
	data []byte
	ref  atomic.Bool
}

// clockShard is the cheap upgrade from FIFO: Borrow stays on the read lock and
// only flips an atomic reference bit (no list mutation), so concurrent faults
// don't serialize. Eviction sweeps the tail giving referenced entries a second
// chance, approximating LRU without the write-lock-on-read cost.
type clockShard struct {
	mu       sync.RWMutex
	maxBytes int64
	bytes    int64
	items    map[pageKey]*list.Element
	lru      *list.List
}

func newClockShard(maxBytes int64) shard {
	return &clockShard{maxBytes: maxBytes, items: map[pageKey]*list.Element{}, lru: list.New()}
}

func (s *clockShard) borrow(k pageKey) ([]byte, bool) {
	s.mu.RLock()
	elem, ok := s.items[k]
	if !ok {
		s.mu.RUnlock()
		return nil, false
	}
	e := elem.Value.(*clockEntry)
	e.ref.Store(true)
	data := e.data
	s.mu.RUnlock()
	return data, true
}

func (s *clockShard) add(k pageKey, data []byte) (bool, int) {
	value := append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()

	if elem, ok := s.items[k]; ok {
		e := elem.Value.(*clockEntry)
		s.bytes += int64(len(value)) - int64(len(e.data))
		e.data = value
		e.ref.Store(true)
		return true, 0
	}

	evicted := 0
	need := int64(len(value))
	for s.bytes+need > s.maxBytes && s.lru.Len() > 0 {
		back := s.lru.Back()
		e := back.Value.(*clockEntry)
		if e.ref.Load() {
			e.ref.Store(false)
			s.lru.MoveToFront(back)
			continue
		}
		delete(s.items, e.key)
		s.bytes -= int64(len(e.data))
		s.lru.Remove(back)
		evicted++
	}
	entry := &clockEntry{key: k, data: value}
	elem := s.lru.PushFront(entry)
	s.items[k] = elem
	s.bytes += need
	return true, evicted
}

func (s *clockShard) internal() (int64, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bytes, len(s.items)
}
