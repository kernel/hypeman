package cachebench

import "sync/atomic"

// shard is one lock-striped partition of a sharded cache. Each policy supplies
// its own shard implementation; the read-path locking inside borrow is what
// determines whether concurrent faults serialize.
type shard interface {
	borrow(k pageKey) ([]byte, bool)
	add(k pageKey, data []byte) (admitted bool, evicted int)
	internal() (bytes int64, items int)
}

type sharded struct {
	name   string
	shards []shard
	mask   uint64

	evictions  atomic.Int64
	admissions atomic.Int64
	rejections atomic.Int64
}

func newSharded(name string, shardCount int, maxBytes int64, factory func(maxBytes int64) shard) *sharded {
	sc := 1
	for sc < shardCount {
		sc <<= 1
	}
	per := maxBytes / int64(sc)
	if per <= 0 {
		per = maxBytes
	}
	shards := make([]shard, sc)
	for i := range shards {
		shards[i] = factory(per)
	}
	return &sharded{name: name, shards: shards, mask: uint64(sc - 1)}
}

func (c *sharded) shardFor(k pageKey) shard {
	if len(c.shards) == 1 {
		return c.shards[0]
	}
	return c.shards[hashKey(k)&c.mask]
}

func (c *sharded) Borrow(cacheKey string, offset int64, size int) ([]byte, bool) {
	k := pageKey{cacheKey, offset, size}
	return c.shardFor(k).borrow(k)
}

func (c *sharded) Add(cacheKey string, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}
	k := pageKey{cacheKey, offset, len(data)}
	admitted, evicted := c.shardFor(k).add(k, data)
	if admitted {
		c.admissions.Add(1)
	} else {
		c.rejections.Add(1)
	}
	if evicted > 0 {
		c.evictions.Add(int64(evicted))
	}
}

func (c *sharded) Internal() InternalStats {
	var bytes int64
	var items int
	for _, s := range c.shards {
		b, n := s.internal()
		bytes += b
		items += n
	}
	return InternalStats{
		Bytes:      bytes,
		Items:      items,
		Evictions:  c.evictions.Load(),
		Admissions: c.admissions.Load(),
		Rejections: c.rejections.Load(),
	}
}

func (c *sharded) Drain()       {}
func (c *sharded) Name() string { return c.name }
