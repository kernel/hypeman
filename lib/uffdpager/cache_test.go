package uffdpager

import (
	"bytes"
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

func TestPageCacheEvictsLRUWhenBounded(t *testing.T) {
	cache := NewPageCache(8192)
	cache.Add("snapshot-a", 0, bytes.Repeat([]byte{1}, 4096))
	cache.Add("snapshot-a", 4096, bytes.Repeat([]byte{2}, 4096))
	if _, ok := cache.Get("snapshot-a", 0, 4096); !ok {
		t.Fatalf("expected first page before eviction")
	}

	cache.Add("snapshot-a", 8192, bytes.Repeat([]byte{3}, 4096))
	if _, ok := cache.Get("snapshot-a", 4096, 4096); ok {
		t.Fatalf("expected least recently used page to be evicted")
	}
	if _, ok := cache.Get("snapshot-a", 0, 4096); !ok {
		t.Fatalf("expected recently used page to remain")
	}
}
