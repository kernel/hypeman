//go:build darwin

package forkvm

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// benchDiskSizeBytes returns the size of the synthetic guest disk copied by the
// fork benchmark. Override with HYPEMAN_BENCH_DISK_BYTES (bytes); defaults to 2 GiB.
func benchDiskSizeBytes(b *testing.B) int64 {
	if v := os.Getenv("HYPEMAN_BENCH_DISK_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			b.Fatalf("invalid HYPEMAN_BENCH_DISK_BYTES %q: %v", v, err)
		}
		return n
	}
	return 2 << 30
}

// writeDenseFile writes sizeBytes of non-zero data so the sparse-copy fallback
// has real work to do (a hole-free extent it must read and rewrite in full).
func writeDenseFile(path string, sizeBytes int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = 0xAB
	}
	for written := int64(0); written < sizeBytes; {
		n := int64(len(buf))
		if rem := sizeBytes - written; rem < n {
			n = rem
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return err
		}
		written += n
	}
	return f.Sync()
}

// BenchmarkForkGuestDisk measures the dominant cost of forking a VM on macOS:
// copying the guest disk. It compares the APFS clonefile(2) fast path against
// the sparse-copy fallback, which is the pre-clonefile behavior (reflink
// disabled). Run with, e.g.:
//
//	go test -run='^$' -bench=BenchmarkForkGuestDisk -benchmem -benchtime=5x ./lib/forkvm/
func BenchmarkForkGuestDisk(b *testing.B) {
	size := benchDiskSizeBytes(b)
	dir := b.TempDir()
	src := filepath.Join(dir, "disk.raw")
	if err := writeDenseFile(src, size); err != nil {
		b.Fatalf("write source disk: %v", err)
	}

	for _, tc := range []struct {
		name     string
		disabled bool
	}{
		{"clonefile", false}, // with the change
		{"sparse", true},     // without the change (reflink fallback)
	} {
		b.Run(tc.name, func(b *testing.B) {
			SetReflinkDisabled(tc.disabled)
			b.Cleanup(func() { SetReflinkDisabled(false) })
			b.SetBytes(size)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst := filepath.Join(dir, fmt.Sprintf("copy-%s-%d.raw", tc.name, i))
				if err := CopyRegularFile(src, dst); err != nil {
					b.Fatalf("copy: %v", err)
				}
				b.StopTimer()
				_ = os.Remove(dst)
				b.StartTimer()
			}
		})
	}
}
