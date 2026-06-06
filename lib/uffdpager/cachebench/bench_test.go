package cachebench

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// defaultWorkload models a host running many VMs forked from a handful of
// backing snapshots with skewed popularity, plus one big one-shot "scan"
// snapshot that streams through unique pages once (the classic cache polluter).
func defaultWorkload() WorkloadConfig {
	snaps := []SnapshotSpec{
		{Key: "snap-0", ImagePages: 32768, CorePages: 512, TailPages: 1536},
		{Key: "snap-1", ImagePages: 32768, CorePages: 512, TailPages: 1536},
		{Key: "snap-2", ImagePages: 32768, CorePages: 512, TailPages: 1536},
		{Key: "snap-3", ImagePages: 32768, CorePages: 512, TailPages: 1536},
		{Key: "snap-4", ImagePages: 32768, CorePages: 512, TailPages: 1536},
		{Key: "snap-5", ImagePages: 32768, CorePages: 512, TailPages: 1536},
		{Key: "scan", ImagePages: 65536, CorePages: 0, TailPages: 8192, OneShot: true},
	}
	return WorkloadConfig{
		Snapshots:   snaps,
		PopZipfS:    1.1,
		NumForks:    1200,
		Concurrency: 24,
		TailZipfS:   1.3,
		Seed:        42,
	}
}

// cacheSizes spans from "much smaller than the working set" to "comfortably
// holds the hot cores" so we can see where policy choice stops mattering.
var cacheSizes = []int64{8 << 20, 16 << 20, 32 << 20, 64 << 20}

// chromeWorkload models the observed production shape: each snapshot's forks
// share a large working set (~800 MB in prod; scaled here so cache/5 ≈ one
// snapshot), and there are many distinct snapshots, so the cache is heavily
// oversubscribed. Scaled by ratio, not absolute size — only cache/working-set,
// overlap, and forks-per-snapshot ratios affect hit rate.
func chromeWorkload(bursty bool) WorkloadConfig {
	const n = 24
	snaps := make([]SnapshotSpec, n)
	for i := range snaps {
		snaps[i] = SnapshotSpec{
			Key:        fmt.Sprintf("chrome-%02d", i),
			ImagePages: 4096, // 16 MiB image
			CorePages:  1536, // ~6 MiB shared core faulted by every fork
			TailPages:  512,  // ~2 MiB lightly-shared tail
		}
	}
	conc := 24 // many snapshots fanning out at once
	if bursty {
		conc = 6 // one snapshot's fan-out mostly completes before the next
	}
	return WorkloadConfig{
		Snapshots:   snaps,
		PopZipfS:    0.6, // mild popularity skew across snapshots
		NumForks:    160,
		Concurrency: conc,
		TailZipfS:   1.3,
		Bursty:      bursty,
		Seed:        7,
	}
}

func TestHitRatesChrome(t *testing.T) {
	sizes := []int64{16 << 20, 40 << 20, 80 << 20, 160 << 20}
	for _, mode := range []struct {
		name   string
		bursty bool
	}{{"concurrent", false}, {"bursty", true}} {
		trace := GenerateTrace(chromeWorkload(mode.bursty))
		t.Logf("=== %s fan-out: %s ===", mode.name, trace)
		for _, mb := range sizes {
			t.Logf("--- cache=%d MiB (~%d snapshots fit) shards=%d ---", mb>>20, mb/(8<<20), benchShards)
			t.Logf("%-10s %8s %12s", "policy", "hit%", "backingRd")
			for _, f := range Factories() {
				p := f.New(mb, benchShards)
				res := Replay(p, trace, 4096)
				t.Logf("%-10s %7.2f%% %12d", f.Name, res.HitRate()*100, res.BackingReads)
			}
		}
	}
}

const benchShards = 16

func TestHitRates(t *testing.T) {
	trace := GenerateTrace(defaultWorkload())
	t.Logf("workload: %s", trace)

	for _, maxBytes := range cacheSizes {
		t.Logf("--- cache = %d MiB (shards=%d) ---", maxBytes>>20, benchShards)
		t.Logf("%-10s %8s %12s %12s %10s", "policy", "hit%", "backingRd", "evictions", "rejects")
		for _, f := range Factories() {
			p := f.New(maxBytes, benchShards)
			res := Replay(p, trace, 4096)
			in := p.Internal()
			t.Logf("%-10s %7.2f%% %12d %12d %10d",
				f.Name, res.HitRate()*100, res.BackingReads, in.Evictions, in.Rejections)
		}
	}
}

// percentile expects sorted nanos.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// TestConcurrencyLatency hammers a small hot key set (all hits) from many
// goroutines and reports read-latency percentiles. This is the "simultaneous
// restores fault the same boot pages" scenario; a policy that mutates state
// under an exclusive lock on read will show its tail blow up here.
func TestConcurrencyLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency test in -short")
	}
	const (
		goroutines  = 64
		opsPerG     = 20000
		hotKeyCount = 256
		maxBytes    = 64 << 20
	)
	hot := makeHotKeys(hotKeyCount)

	type variant struct {
		name   string
		shards int
		mk     func() Policy
	}
	variants := []variant{
		{"prod", 0, func() Policy { return newProd(maxBytes) }},
		{"fifo/16", 16, func() Policy { return newSharded("fifo", 16, maxBytes, newFIFOShard) }},
		{"lru/1", 1, func() Policy { return newSharded("lru", 1, maxBytes, newLRUShard) }},
		{"lru/16", 16, func() Policy { return newSharded("lru", 16, maxBytes, newLRUShard) }},
		{"lru/64", 64, func() Policy { return newSharded("lru", 64, maxBytes, newLRUShard) }},
		{"clock/16", 16, func() Policy { return newSharded("clock", 16, maxBytes, newClockShard) }},
		{"tinylfu/16", 16, func() Policy { return newSharded("tinylfu", 16, maxBytes, newTinyLFUShard) }},
		{"ristretto", 0, func() Policy { return newRistretto(maxBytes) }},
	}

	t.Logf("%d goroutines x %d ops over %d hot keys (all hits)", goroutines, opsPerG, hotKeyCount)
	t.Logf("%-12s %10s %10s %10s %10s %12s", "policy", "p50(ns)", "p99(ns)", "p999(ns)", "max(ns)", "ops/sec")
	for _, v := range variants {
		p := v.mk()
		for _, op := range hot {
			p.Add(op.key, op.offset, make([]byte, pageSize))
		}
		p.Drain()

		lat := make([][]int64, goroutines)
		var wg sync.WaitGroup
		var seedCtr uint64
		start := time.Now()
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				ls := make([]int64, opsPerG)
				x := atomic.AddUint64(&seedCtr, 0x9e3779b97f4a7c15)
				n := uint64(len(hot))
				for i := 0; i < opsPerG; i++ {
					x ^= x << 13
					x ^= x >> 7
					x ^= x << 17
					op := hot[x%n]
					t0 := time.Now()
					p.Borrow(op.key, op.offset, pageSize)
					ls[i] = time.Since(t0).Nanoseconds()
				}
				lat[g] = ls
			}(g)
		}
		wg.Wait()
		elapsed := time.Since(start)

		all := make([]int64, 0, goroutines*opsPerG)
		for _, ls := range lat {
			all = append(all, ls...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		opsPerSec := float64(len(all)) / elapsed.Seconds()
		t.Logf("%-12s %10d %10d %10d %10d %12.0f",
			v.name,
			percentile(all, 0.50), percentile(all, 0.99),
			percentile(all, 0.999), all[len(all)-1], opsPerSec)
	}
}

func makeHotKeys(n int) []faultOp {
	out := make([]faultOp, n)
	for i := 0; i < n; i++ {
		out[i] = faultOp{key: "snap-hot", offset: int64(i) * pageSize}
	}
	return out
}

// BenchmarkReadHot measures concurrent read throughput on a small hot set (all
// hits). ns/op that rises with -cpu means the read path serializes.
func BenchmarkReadHot(b *testing.B) {
	const (
		hotKeyCount = 256
		maxBytes    = 64 << 20
	)
	hot := makeHotKeys(hotKeyCount)
	shardCounts := []int{1, 16, 64, 256}

	for _, f := range Factories() {
		if f.Sharded {
			for _, sc := range shardCounts {
				b.Run(fmt.Sprintf("%s/shards=%d", f.Name, sc), func(b *testing.B) {
					benchReadParallel(b, f.New(maxBytes, sc), hot)
				})
			}
		} else {
			b.Run(f.Name, func(b *testing.B) {
				benchReadParallel(b, f.New(maxBytes, 0), hot)
			})
		}
	}
}

func benchReadParallel(b *testing.B, p Policy, hot []faultOp) {
	for _, op := range hot {
		p.Add(op.key, op.offset, make([]byte, pageSize))
	}
	p.Drain()
	var seedCtr uint64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		x := atomic.AddUint64(&seedCtr, 0x9e3779b97f4a7c15)
		n := uint64(len(hot))
		for pb.Next() {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			op := hot[x%n]
			p.Borrow(op.key, op.offset, pageSize)
		}
	})
}

// BenchmarkMixed adds a write/eviction component: ~90% hot reads, ~10% inserts
// of fresh churn pages that force eviction, exercising the Add path under
// concurrency too.
func BenchmarkMixed(b *testing.B) {
	const (
		hotKeyCount = 512
		churnRange  = 1 << 16
		maxBytes    = 32 << 20
	)
	hot := makeHotKeys(hotKeyCount)
	shardCounts := []int{16, 64}

	run := func(b *testing.B, p Policy) {
		for _, op := range hot {
			p.Add(op.key, op.offset, make([]byte, pageSize))
		}
		p.Drain()
		var seedCtr uint64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			x := atomic.AddUint64(&seedCtr, 0x9e3779b97f4a7c15)
			n := uint64(len(hot))
			i := 0
			for pb.Next() {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				if i%10 == 9 {
					off := int64(x%churnRange) * pageSize
					p.Add("churn", off, make([]byte, pageSize))
				} else {
					op := hot[x%n]
					p.Borrow(op.key, op.offset, pageSize)
				}
				i++
			}
		})
	}

	for _, f := range Factories() {
		if f.Sharded {
			for _, sc := range shardCounts {
				b.Run(fmt.Sprintf("%s/shards=%d", f.Name, sc), func(b *testing.B) {
					run(b, f.New(maxBytes, sc))
				})
			}
		} else {
			b.Run(f.Name, func(b *testing.B) { run(b, f.New(maxBytes, 0)) })
		}
	}
}
