package cachebench

import "github.com/kernel/hypeman/lib/uffdpager"

// prodPolicy wraps the actual shipping pager cache so the experiment compares
// against real behavior, not just a reimplementation of it.
type prodPolicy struct {
	pc *uffdpager.PageCache
}

func newProd(maxBytes int64) Policy {
	return &prodPolicy{pc: uffdpager.NewPageCache(maxBytes)}
}

func (p *prodPolicy) Borrow(cacheKey string, offset int64, size int) ([]byte, bool) {
	return p.pc.Borrow(cacheKey, offset, size)
}

func (p *prodPolicy) Add(cacheKey string, offset int64, data []byte) {
	p.pc.Add(cacheKey, offset, data)
}

func (p *prodPolicy) Internal() InternalStats {
	bytes, maxBytes, items, _, _ := p.pc.SnapshotStats()
	return InternalStats{Bytes: bytes, MaxBytes: maxBytes, Items: items}
}

func (p *prodPolicy) Drain()       {}
func (p *prodPolicy) Name() string { return "prod" }
