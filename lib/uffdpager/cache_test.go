package uffdpager

import (
	"bytes"
	"sync"
	"testing"
)

func TestPageCacheSharesPagesByCacheKeyAndOffset(t *testing.T) {
	cache := NewPageCache(8192)
	page := bytes.Repeat([]byte{7}, 4096)

	cache.Add("snapshot-a", 0, page)
	got, ok := cache.Get("snapshot-a", 0, 4096)
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if !bytes.Equal(got, page) {
		t.Fatalf("cached page mismatch")
	}

	got[0] = 1
	again, ok := cache.Get("snapshot-a", 0, 4096)
	if !ok {
		t.Fatalf("expected second cache hit")
	}
	if again[0] != 7 {
		t.Fatalf("cache returned mutable backing slice")
	}
}

func TestPageCacheSecondChanceEvictsUnreferenced(t *testing.T) {
	// Two-page shard. Insert three pages so the first eviction sweep clears the
	// ref bit on every survivor. Re-touch one survivor, then insert a fourth —
	// the untouched one must be the victim.
	cache := NewPageCache(8192)
	cache.Add("snapshot-a", 0, bytes.Repeat([]byte{1}, 4096))
	cache.Add("snapshot-a", 4096, bytes.Repeat([]byte{2}, 4096))
	cache.Add("snapshot-a", 8192, bytes.Repeat([]byte{3}, 4096))

	if _, ok := cache.Get("snapshot-a", 8192, 4096); !ok {
		t.Fatalf("expected page 8192 to survive first eviction")
	}

	cache.Add("snapshot-a", 12288, bytes.Repeat([]byte{4}, 4096))

	if _, ok := cache.Get("snapshot-a", 4096, 4096); ok {
		t.Fatalf("expected unreferenced page to be evicted")
	}
	if _, ok := cache.Get("snapshot-a", 8192, 4096); !ok {
		t.Fatalf("expected referenced page to remain")
	}
}

func TestPageCacheNewEntrySurvivesFullyReferencedShard(t *testing.T) {
	// Regression: when every existing page is referenced, a newly inserted
	// page must still get a grace pass. Without ref=true on insert, the sweep
	// relocates each spared entry ahead of the new page, leaving the new page
	// at the back with ref=false and evicting it before any fault can hit it.
	cache := NewPageCache(8192)
	cache.Add("snapshot-a", 0, bytes.Repeat([]byte{1}, 4096))
	cache.Add("snapshot-a", 4096, bytes.Repeat([]byte{2}, 4096))
	if _, ok := cache.Get("snapshot-a", 0, 4096); !ok {
		t.Fatalf("expected page 0 to be present")
	}
	if _, ok := cache.Get("snapshot-a", 4096, 4096); !ok {
		t.Fatalf("expected page 4096 to be present")
	}

	cache.Add("snapshot-a", 8192, bytes.Repeat([]byte{3}, 4096))

	if _, ok := cache.Get("snapshot-a", 8192, 4096); !ok {
		t.Fatalf("newly inserted page should survive an eviction sweep where every other entry was referenced")
	}
}

func TestPageCacheConcurrentBorrowIsRaceFree(t *testing.T) {
	// All Borrowers take the shard read lock and concurrently store into
	// cacheEntry.ref. -race must not flag this — the bit is atomic precisely
	// so the hot path can stay on RLock.
	cache := NewPageCache(4096)
	cache.Add("snapshot-a", 0, bytes.Repeat([]byte{7}, 4096))

	const goroutines = 32
	const iterations = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range iterations {
				data, ok := cache.Borrow("snapshot-a", 0, 4096)
				if !ok {
					t.Errorf("expected hit on hot page")
					return
				}
				if data[0] != 7 {
					t.Errorf("unexpected page contents %d", data[0])
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPageCacheDistributesAlignedPagesAcrossShards(t *testing.T) {
	cache := NewPageCache(4 << 30)
	page := bytes.Repeat([]byte{1}, 4096)

	for i := range 4096 {
		cache.Add("snapshot-a", int64(i*4096), page)
	}

	usedShards := 0
	for _, shard := range cache.shards {
		shard.mu.Lock()
		if len(shard.items) > 0 {
			usedShards++
		}
		shard.mu.Unlock()
	}
	if usedShards < len(cache.shards)/2 {
		t.Fatalf("expected aligned pages to spread across shards, used %d of %d", usedShards, len(cache.shards))
	}
}
