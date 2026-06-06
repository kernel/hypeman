package cachebench

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// SnapshotSpec describes one backing snapshot. Every fork of it re-faults the
// same CorePages (the shared boot/runtime working set that gives the cache its
// value), plus TailPages drawn Zipfian over the rest of the image (mostly
// per-fork-unique, lightly shared).
type SnapshotSpec struct {
	Key        string
	ImagePages int
	CorePages  int
	TailPages  int
	OneShot    bool // faulted by a single fork: models a scan that can pollute
}

type WorkloadConfig struct {
	Snapshots   []SnapshotSpec
	PopZipfS    float64 // snapshot popularity skew (higher = a few hot snapshots)
	NumForks    int
	Concurrency int     // forks faulting at once; models simultaneous restores
	TailZipfS   float64 // offset skew within a snapshot's tail
	Bursty      bool    // group a snapshot's forks together (sequential fan-out) vs scatter them (concurrent fan-out across snapshots)
	Seed        int64
}

type faultOp struct {
	key    string
	offset int64
}

type forkStream struct {
	key  string
	offs []int64
}

// Trace is a replayable, deterministic fault sequence already interleaved
// across concurrent forks.
type Trace struct {
	Ops         []faultOp
	UniquePages int
	UniqueBytes int64
	Cfg         WorkloadConfig
	ForksByKey  map[string]int
}

func zipfWeights(n int, s float64) []float64 {
	w := make([]float64, n)
	var sum float64
	for i := 0; i < n; i++ {
		w[i] = 1.0 / math.Pow(float64(i+1), s)
		sum += w[i]
	}
	for i := range w {
		w[i] /= sum
	}
	return w
}

// GenerateTrace builds the full interleaved fault trace deterministically.
func GenerateTrace(cfg WorkloadConfig) *Trace {
	rng := rand.New(rand.NewSource(cfg.Seed))

	// Popularity over multi-shot snapshots; one-shots get exactly one fork.
	var multi []int
	for i, s := range cfg.Snapshots {
		if !s.OneShot {
			multi = append(multi, i)
		}
	}
	weights := zipfWeights(len(multi), cfg.PopZipfS)

	tailS := cfg.TailZipfS
	if tailS <= 1 {
		tailS = 1.01 // rand.NewZipf requires s > 1
	}
	tailZipf := map[string]*rand.Zipf{}
	for _, s := range cfg.Snapshots {
		span := s.ImagePages - s.CorePages
		if span < 2 {
			span = 2
		}
		tailZipf[s.Key] = rand.NewZipf(rng, tailS, 1, uint64(span-1))
	}

	mkStream := func(s SnapshotSpec) forkStream {
		offs := make([]int64, 0, s.CorePages+s.TailPages)
		for i := 0; i < s.CorePages; i++ {
			offs = append(offs, int64(i)*pageSize)
		}
		z := tailZipf[s.Key]
		for i := 0; i < s.TailPages; i++ {
			page := s.CorePages + int(z.Uint64())
			if page >= s.ImagePages {
				page = s.ImagePages - 1
			}
			offs = append(offs, int64(page)*pageSize)
		}
		rng.Shuffle(len(offs), func(a, b int) { offs[a], offs[b] = offs[b], offs[a] })
		return forkStream{key: s.Key, offs: offs}
	}

	// Assemble the list of forks: weighted picks for multi-shot snapshots, plus
	// one fork for each one-shot snapshot inserted partway through.
	streams := make([]forkStream, 0, cfg.NumForks)
	forksByKey := map[string]int{}
	for f := 0; f < cfg.NumForks; f++ {
		idx := multi[weightedPick(rng, weights)]
		s := cfg.Snapshots[idx]
		streams = append(streams, mkStream(s))
		forksByKey[s.Key]++
	}
	for _, s := range cfg.Snapshots {
		if !s.OneShot {
			continue
		}
		pos := rng.Intn(len(streams) + 1)
		streams = append(streams, forkStream{})
		copy(streams[pos+1:], streams[pos:])
		streams[pos] = mkStream(s)
		forksByKey[s.Key]++
	}

	if cfg.Bursty {
		// Group each snapshot's forks together so its fan-out happens as one
		// burst, rather than scattered among other snapshots' forks.
		sort.SliceStable(streams, func(a, b int) bool { return streams[a].key < streams[b].key })
	}

	ops := interleave(streams, cfg.Concurrency)

	uniq := map[faultOp]struct{}{}
	for _, op := range ops {
		uniq[op] = struct{}{}
	}

	return &Trace{
		Ops:         ops,
		UniquePages: len(uniq),
		UniqueBytes: int64(len(uniq)) * pageSize,
		Cfg:         cfg,
		ForksByKey:  forksByKey,
	}
}

// interleave round-robins faults across up to `concurrency` active forks so the
// access order reflects many VMs restoring at the same time, rather than one
// fork fully completing before the next starts.
func interleave(streams []forkStream, concurrency int) []faultOp {
	if concurrency < 1 {
		concurrency = 1
	}
	total := 0
	for _, s := range streams {
		total += len(s.offs)
	}
	ops := make([]faultOp, 0, total)

	type cursor struct {
		key string
		off []int64
		pos int
	}
	next := 0
	active := make([]*cursor, 0, concurrency)
	admit := func() {
		for len(active) < concurrency && next < len(streams) {
			s := streams[next]
			next++
			if len(s.offs) == 0 {
				continue
			}
			active = append(active, &cursor{key: s.key, off: s.offs})
		}
	}
	admit()
	for len(active) > 0 {
		alive := active[:0]
		for _, c := range active {
			ops = append(ops, faultOp{key: c.key, offset: c.off[c.pos]})
			c.pos++
			if c.pos < len(c.off) {
				alive = append(alive, c)
			}
		}
		active = alive
		admit()
	}
	return ops
}

func weightedPick(rng *rand.Rand, weights []float64) int {
	x := rng.Float64()
	var cum float64
	for i, w := range weights {
		cum += w
		if x < cum {
			return i
		}
	}
	return len(weights) - 1
}

// String renders a one-line summary of the workload for test output.
func (t *Trace) String() string {
	keys := make([]string, 0, len(t.ForksByKey))
	for k := range t.ForksByKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Sprintf("forks=%d ops=%d uniquePages=%d (%.1f MiB) concurrency=%d snapshots=%d",
		sumForks(t.ForksByKey), len(t.Ops), t.UniquePages,
		float64(t.UniqueBytes)/(1<<20), t.Cfg.Concurrency, len(keys))
}

func sumForks(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
