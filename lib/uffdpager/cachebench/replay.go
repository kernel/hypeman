package cachebench

// ReplayResult is the outcome of running a trace through one policy.
type ReplayResult struct {
	Hits         int64
	Misses       int64
	BackingReads int64 // == Misses; the disk reads the cache failed to avoid
}

func (r ReplayResult) HitRate() float64 {
	total := r.Hits + r.Misses
	if total == 0 {
		return 0
	}
	return float64(r.Hits) / float64(total)
}

// Replay drives a policy through the trace exactly as the pager would: Borrow
// on each fault, and on a miss "read from backing" (allocate a page) and Add.
// drainEvery flushes any async admission periodically so policies like
// ristretto reach a representative steady state during single-threaded replay.
func Replay(p Policy, trace *Trace, drainEvery int) ReplayResult {
	var res ReplayResult
	for i, op := range trace.Ops {
		if _, ok := p.Borrow(op.key, op.offset, pageSize); ok {
			res.Hits++
		} else {
			res.Misses++
			res.BackingReads++
			p.Add(op.key, op.offset, make([]byte, pageSize))
		}
		if drainEvery > 0 && i%drainEvery == 0 {
			p.Drain()
		}
	}
	p.Drain()
	return res
}
