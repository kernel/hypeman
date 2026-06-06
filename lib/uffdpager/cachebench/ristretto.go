package cachebench

import (
	"strconv"

	"github.com/dgraph-io/ristretto/v2"
)

// ristrettoPolicy wraps dgraph's ristretto, a production W-TinyLFU cache with a
// lock-light read path (access events are batched through a ring buffer rather
// than mutating a list under lock). This is the "full" frequency-aware
// reference point, including the recency window the hand-rolled tinylfu omits.
type ristrettoPolicy struct {
	c *ristretto.Cache[string, []byte]
}

func newRistretto(maxBytes int64) Policy {
	maxItems := maxBytes / pageSize
	counters := maxItems * 10
	if counters < 1000 {
		counters = 1000
	}
	c, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: counters,
		MaxCost:     maxBytes,
		BufferItems: 64,
		Metrics:     true,
	})
	if err != nil {
		panic(err)
	}
	return &ristrettoPolicy{c: c}
}

func rkey(cacheKey string, offset int64) string {
	return cacheKey + "@" + strconv.FormatInt(offset, 10)
}

func (p *ristrettoPolicy) Borrow(cacheKey string, offset int64, size int) ([]byte, bool) {
	return p.c.Get(rkey(cacheKey, offset))
}

func (p *ristrettoPolicy) Add(cacheKey string, offset int64, data []byte) {
	p.c.Set(rkey(cacheKey, offset), data, int64(len(data)))
}

func (p *ristrettoPolicy) Internal() InternalStats {
	m := p.c.Metrics
	if m == nil {
		return InternalStats{}
	}
	return InternalStats{
		Bytes:      int64(m.CostAdded() - m.CostEvicted()),
		Items:      int(m.KeysAdded() - m.KeysEvicted()),
		Evictions:  int64(m.KeysEvicted()),
		Admissions: int64(m.KeysAdded()),
	}
}

func (p *ristrettoPolicy) Drain()       { p.c.Wait() }
func (p *ristrettoPolicy) Name() string { return "ristretto" }
