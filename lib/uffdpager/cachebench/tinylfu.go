package cachebench

import (
	"container/list"
	"math"
	"sync"
	"sync/atomic"
)

const cmDepth = 4

var cmSeeds = [cmDepth]uint64{
	0x9E3779B97F4A7C15, 0xC2B2AE3D27D4EB4F,
	0x165667B19E3779F9, 0x27D4EB2F165667C5,
}

// cmSketch is a Count-Min sketch with periodic halving (aging). Counters are
// updated with atomic ops so the read path can record frequency without taking
// the shard's write lock. A benign check-then-increment race only ever
// under-counts slightly, which the sketch already tolerates.
type cmSketch struct {
	counters  []uint32
	width     uint64
	mask      uint64
	additions atomic.Int64
	resetAt   int64
	resetMu   sync.Mutex
}

func newCMSketch(maxItems int) *cmSketch {
	width := uint64(16)
	for width < uint64(maxItems) {
		width <<= 1
	}
	return &cmSketch{
		counters: make([]uint32, int(width)*cmDepth),
		width:    width,
		mask:     width - 1,
		resetAt:  int64(maxItems) * 10,
	}
}

func (s *cmSketch) idx(row int, h uint64) uint64 {
	hh := (h ^ cmSeeds[row]) * cmSeeds[row]
	return uint64(row)*s.width + (hh & s.mask)
}

func (s *cmSketch) increment(h uint64) {
	for r := 0; r < cmDepth; r++ {
		p := &s.counters[s.idx(r, h)]
		for {
			c := atomic.LoadUint32(p)
			if c >= 255 {
				break
			}
			if atomic.CompareAndSwapUint32(p, c, c+1) {
				break
			}
		}
	}
	if s.additions.Add(1) >= s.resetAt {
		s.maybeReset()
	}
}

func (s *cmSketch) estimate(h uint64) uint32 {
	min := uint32(math.MaxUint32)
	for r := 0; r < cmDepth; r++ {
		if c := atomic.LoadUint32(&s.counters[s.idx(r, h)]); c < min {
			min = c
		}
	}
	return min
}

func (s *cmSketch) maybeReset() {
	if s.additions.Load() < s.resetAt {
		return
	}
	s.resetMu.Lock()
	defer s.resetMu.Unlock()
	if s.additions.Load() < s.resetAt {
		return
	}
	for i := range s.counters {
		atomic.StoreUint32(&s.counters[i], atomic.LoadUint32(&s.counters[i])>>1)
	}
	s.additions.Store(0)
}

// tinylfuShard is a FIFO-ordered main store fronted by a TinyLFU admission
// filter. Borrow records frequency in the sketch (lock-free) and reads under
// RLock — no list reorder, so concurrent faults stay parallel.
//
// It is W-TinyLFU: a small recency window admits every newcomer first (so a
// freshly-faulted hot page isn't rejected before it can build frequency — the
// cold-start trap of admission-without-a-window), and pages evicted from the
// window only enter the main segment if their estimated frequency beats the
// main victim's. That admission test is what keeps one-shot scan pages from
// displacing hot shared pages.
type tinylfuShard struct {
	mu        sync.RWMutex
	winMax    int64
	winBytes  int64
	mainMax   int64
	mainBytes int64
	win       map[pageKey]*list.Element
	winList   *list.List
	main      map[pageKey]*list.Element
	mainList  *list.List
	sketch    *cmSketch
}

func newTinyLFUShard(maxBytes int64) shard {
	maxItems := int(maxBytes / pageSize)
	if maxItems < 16 {
		maxItems = 16
	}
	winMax := maxBytes / 100 // ~1% recency window, per the W-TinyLFU default
	if winMax < pageSize {
		winMax = pageSize
	}
	mainMax := maxBytes - winMax
	if mainMax < pageSize {
		mainMax = pageSize
	}
	return &tinylfuShard{
		winMax:   winMax,
		mainMax:  mainMax,
		win:      map[pageKey]*list.Element{},
		winList:  list.New(),
		main:     map[pageKey]*list.Element{},
		mainList: list.New(),
		sketch:   newCMSketch(maxItems),
	}
}

func (s *tinylfuShard) borrow(k pageKey) ([]byte, bool) {
	s.sketch.increment(hashKey(k))
	s.mu.RLock()
	if elem, ok := s.main[k]; ok {
		data := elem.Value.(*lruEntry).data
		s.mu.RUnlock()
		return data, true
	}
	if elem, ok := s.win[k]; ok {
		data := elem.Value.(*lruEntry).data
		s.mu.RUnlock()
		return data, true
	}
	s.mu.RUnlock()
	return nil, false
}

func (s *tinylfuShard) add(k pageKey, data []byte) (bool, int) {
	value := append([]byte(nil), data...)
	need := int64(len(value))
	s.mu.Lock()
	defer s.mu.Unlock()

	if elem, ok := s.main[k]; ok {
		e := elem.Value.(*lruEntry)
		s.mainBytes += need - int64(len(e.data))
		e.data = value
		return true, 0
	}
	if elem, ok := s.win[k]; ok {
		e := elem.Value.(*lruEntry)
		s.winBytes += need - int64(len(e.data))
		e.data = value
		return true, 0
	}

	elem := s.winList.PushFront(&lruEntry{key: k, data: value})
	s.win[k] = elem
	s.winBytes += need

	evicted := 0
	for s.winBytes > s.winMax && s.winList.Len() > 0 {
		back := s.winList.Back()
		cand := back.Value.(*lruEntry)
		s.winList.Remove(back)
		delete(s.win, cand.key)
		s.winBytes -= int64(len(cand.data))
		evicted += s.admitToMain(cand)
	}
	return true, evicted
}

// admitToMain offers a window-evicted candidate to the main segment. Pages are
// uniform size, so freeing one main entry makes room for one candidate.
func (s *tinylfuShard) admitToMain(cand *lruEntry) int {
	need := int64(len(cand.data))
	if s.mainBytes+need <= s.mainMax || s.mainList.Len() == 0 {
		elem := s.mainList.PushFront(cand)
		s.main[cand.key] = elem
		s.mainBytes += need
		return 0
	}
	victim := s.mainList.Back().Value.(*lruEntry)
	if s.sketch.estimate(hashKey(cand.key)) <= s.sketch.estimate(hashKey(victim.key)) {
		return 0 // candidate loses the frequency contest; drop it
	}
	evicted := 0
	for s.mainBytes+need > s.mainMax && s.mainList.Len() > 0 {
		back := s.mainList.Back()
		e := back.Value.(*lruEntry)
		s.mainList.Remove(back)
		delete(s.main, e.key)
		s.mainBytes -= int64(len(e.data))
		evicted++
	}
	elem := s.mainList.PushFront(cand)
	s.main[cand.key] = elem
	s.mainBytes += need
	return evicted
}

func (s *tinylfuShard) internal() (int64, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.winBytes + s.mainBytes, len(s.win) + len(s.main)
}
