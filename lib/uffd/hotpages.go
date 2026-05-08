package uffd

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// HotPage points at a single page-aligned location inside a registered
// memory region. Region is the index into the handshake's mappings list;
// PageOffset is the byte offset of the page within that region (always
// a multiple of the server's page size).
type HotPage struct {
	Region     uint32
	PageOffset uint64
}

// HotPageList is the persisted "what pages should we eagerly populate
// before the guest unpauses" list. PR 8 records one of these during a
// template's first fork warm-up and bakes it into Template.HotPagesPath;
// later forks call Server.Prefetch with the loaded list to skip the
// fault round-trips on those pages.
//
// Concurrent Add/Snapshot is safe; Save and Load are not — callers
// generally Save once at the end of warmup and Load once at boot.
type HotPageList struct {
	mu    sync.Mutex
	pages []HotPage
}

// hotPagesFileMagic prefixes saved files so we can refuse to load
// arbitrary garbage. The version byte exists so a future format change
// can be rejected loudly instead of silently misinterpreted.
var hotPagesFileMagic = []byte("HPL1")

// Add records a single hot page. Duplicates are tolerated; Snapshot
// dedups before returning.
func (h *HotPageList) Add(p HotPage) {
	h.mu.Lock()
	h.pages = append(h.pages, p)
	h.mu.Unlock()
}

// Len returns the number of recorded pages (with duplicates).
func (h *HotPageList) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pages)
}

// Snapshot returns a sorted, deduplicated copy of the recorded pages.
// Sort order is (Region, PageOffset) so prefetch issues sequential
// reads against the template mem-file.
func (h *HotPageList) Snapshot() []HotPage {
	h.mu.Lock()
	src := make([]HotPage, len(h.pages))
	copy(src, h.pages)
	h.mu.Unlock()

	sort.Slice(src, func(i, j int) bool {
		if src[i].Region != src[j].Region {
			return src[i].Region < src[j].Region
		}
		return src[i].PageOffset < src[j].PageOffset
	})
	out := src[:0]
	var last HotPage
	for i, p := range src {
		if i == 0 || p != last {
			out = append(out, p)
			last = p
		}
	}
	return out
}

// Save atomically writes the deduplicated snapshot to path. The format
// is: 4-byte magic ("HPL1"), uvarint count, then for each page a
// uvarint region index and a uvarint page offset. Atomic via tmp+rename.
func (h *HotPageList) Save(path string) error {
	pages := h.Snapshot()
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("uffd: create hot pages tmp: %w", err)
	}
	bw := bufio.NewWriter(f)
	if _, err := bw.Write(hotPagesFileMagic); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("uffd: write hot pages magic: %w", err)
	}
	var ibuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(ibuf[:], uint64(len(pages)))
	if _, err := bw.Write(ibuf[:n]); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("uffd: write hot pages count: %w", err)
	}
	for _, p := range pages {
		n = binary.PutUvarint(ibuf[:], uint64(p.Region))
		if _, err := bw.Write(ibuf[:n]); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("uffd: write hot pages region: %w", err)
		}
		n = binary.PutUvarint(ibuf[:], p.PageOffset)
		if _, err := bw.Write(ibuf[:n]); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("uffd: write hot pages offset: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("uffd: flush hot pages: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("uffd: close hot pages tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("uffd: rename hot pages: %w", err)
	}
	return nil
}

// LoadHotPageList reads a HotPageList from path. Returns an empty list
// (not an error) when path does not exist; the absence of a baked
// hot-page file simply means "don't prefetch."
func LoadHotPageList(path string) (*HotPageList, error) {
	clean := filepath.Clean(path)
	data, err := os.ReadFile(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &HotPageList{}, nil
		}
		return nil, fmt.Errorf("uffd: read hot pages: %w", err)
	}
	if len(data) < len(hotPagesFileMagic) {
		return nil, errors.New("uffd: hot pages file truncated")
	}
	if string(data[:len(hotPagesFileMagic)]) != string(hotPagesFileMagic) {
		return nil, errors.New("uffd: hot pages file has bad magic")
	}
	rest := data[len(hotPagesFileMagic):]
	count, n := binary.Uvarint(rest)
	if n <= 0 {
		return nil, errors.New("uffd: hot pages file has bad count")
	}
	rest = rest[n:]
	out := &HotPageList{pages: make([]HotPage, 0, count)}
	for i := uint64(0); i < count; i++ {
		region, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("uffd: hot pages file truncated at entry %d (region)", i)
		}
		rest = rest[n:]
		offset, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("uffd: hot pages file truncated at entry %d (offset)", i)
		}
		rest = rest[n:]
		out.pages = append(out.pages, HotPage{Region: uint32(region), PageOffset: offset})
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("uffd: hot pages file has %d trailing bytes", len(rest))
	}
	return out, nil
}
