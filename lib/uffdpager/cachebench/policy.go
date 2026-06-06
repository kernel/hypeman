// Package cachebench is a throwaway experiment harness for comparing page-cache
// eviction policies for the UFFD pager. It is not wired into the pager; it
// exists only to measure hit-rate and concurrency behavior under a workload
// that models many VMs restoring from a few shared backing snapshots.
package cachebench

const pageSize = 4096

type pageKey struct {
	cacheKey string
	offset   int64
	size     int
}

// Policy is the common surface every candidate cache implements. Borrow/Add
// mirror the pager's hot path: Borrow on a fault, Add after reading the page
// from the backing file on a miss.
type Policy interface {
	Borrow(cacheKey string, offset int64, size int) ([]byte, bool)
	Add(cacheKey string, offset int64, data []byte)
	Internal() InternalStats
	// Drain blocks until any asynchronous admission has settled. No-op for
	// synchronous policies; ristretto needs it to make hit-rate measurable.
	Drain()
	Name() string
}

// InternalStats is best-effort; fields a policy can't cheaply report stay zero.
type InternalStats struct {
	Bytes      int64
	MaxBytes   int64
	Items      int
	Evictions  int64
	Admissions int64
	Rejections int64
}

func hashKey(k pageKey) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(k.cacheKey); i++ {
		h ^= uint64(k.cacheKey[i])
		h *= prime64
	}
	h ^= uint64(k.offset)
	h *= prime64
	h ^= uint64(k.offset >> 32)
	h *= prime64
	h ^= uint64(k.size)
	h *= prime64
	return mix(h)
}

func mix(h uint64) uint64 {
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
