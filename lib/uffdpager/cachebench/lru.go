package cachebench

import (
	"container/list"
	"sync"
)

type lruEntry struct {
	key  pageKey
	data []byte
}

// lruLikeShard backs both the FIFO baseline (touch=false, matching the current
// production cache where Borrow does not reorder) and true LRU (touch=true,
// Borrow moves to front). The difference is one bool — and the lock it forces:
// FIFO reads under RLock, LRU reads under the exclusive lock because
// MoveToFront mutates the list.
type lruLikeShard struct {
	mu       sync.RWMutex
	maxBytes int64
	bytes    int64
	items    map[pageKey]*list.Element
	lru      *list.List
	touch    bool
}

func newFIFOShard(maxBytes int64) shard {
	return &lruLikeShard{maxBytes: maxBytes, items: map[pageKey]*list.Element{}, lru: list.New(), touch: false}
}

func newLRUShard(maxBytes int64) shard {
	return &lruLikeShard{maxBytes: maxBytes, items: map[pageKey]*list.Element{}, lru: list.New(), touch: true}
}

func (s *lruLikeShard) borrow(k pageKey) ([]byte, bool) {
	if s.touch {
		s.mu.Lock()
		defer s.mu.Unlock()
		elem, ok := s.items[k]
		if !ok {
			return nil, false
		}
		s.lru.MoveToFront(elem)
		return elem.Value.(*lruEntry).data, true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	elem, ok := s.items[k]
	if !ok {
		return nil, false
	}
	return elem.Value.(*lruEntry).data, true
}

func (s *lruLikeShard) add(k pageKey, data []byte) (bool, int) {
	value := append([]byte(nil), data...)
	s.mu.Lock()
	defer s.mu.Unlock()

	if elem, ok := s.items[k]; ok {
		e := elem.Value.(*lruEntry)
		s.bytes += int64(len(value)) - int64(len(e.data))
		e.data = value
		s.lru.MoveToFront(elem)
		return true, 0
	}

	evicted := 0
	need := int64(len(value))
	for s.bytes+need > s.maxBytes && s.lru.Len() > 0 {
		back := s.lru.Back()
		e := back.Value.(*lruEntry)
		delete(s.items, e.key)
		s.bytes -= int64(len(e.data))
		s.lru.Remove(back)
		evicted++
	}
	elem := s.lru.PushFront(&lruEntry{key: k, data: value})
	s.items[k] = elem
	s.bytes += need
	return true, evicted
}

func (s *lruLikeShard) internal() (int64, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bytes, len(s.items)
}
