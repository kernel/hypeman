//go:build linux

package diskutilization

import (
	"os"
	"sort"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fsIOCFiemap        = 0xC020660B
	fiemapFlagSync     = 0x1
	fiemapExtentLast   = 0x1
	fiemapExtentShared = 0x2000
	fiemapMaxExtents   = 256
	fiemapWholeFile    = ^uint64(0)
)

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type fiemapRequest struct {
	Header  fiemapHeader
	Extents [fiemapMaxExtents]fiemapExtent
}

type sharedExtentTracker struct {
	ranges []physicalRange
}

type physicalRange struct {
	start uint64
	end   uint64
}

func newSharedExtentTracker() *sharedExtentTracker {
	return &sharedExtentTracker{}
}

func snapshotAllocatedBytesForPath(path string, sharedExtents *sharedExtentTracker) (int64, int64) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0
	}
	if !info.Mode().IsRegular() {
		return allocatedBytesForPath(path), 0
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	allocatedBytes := stat.Blocks * 512
	if allocatedBytes == 0 {
		return 0, 0
	}

	privateBytes, sharedBytes, sawShared, err := fiemapAllocatedBytes(path, sharedExtents)
	if err != nil || !sawShared {
		return allocatedBytes, 0
	}
	return privateBytes, sharedBytes
}

func fiemapAllocatedBytes(path string, sharedExtents *sharedExtentTracker) (int64, int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false, err
	}
	defer f.Close()

	var privateBytes int64
	var sharedBytes int64
	var sawShared bool
	start := uint64(0)

	for {
		var req fiemapRequest
		req.Header.Start = start
		req.Header.Length = fiemapWholeFile
		req.Header.Flags = fiemapFlagSync
		req.Header.ExtentCount = fiemapMaxExtents

		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(fsIOCFiemap), uintptr(unsafe.Pointer(&req))); errno != 0 {
			return 0, 0, false, errno
		}
		if req.Header.MappedExtents == 0 {
			break
		}

		extents := req.Extents[:req.Header.MappedExtents]
		for _, extent := range extents {
			if extent.Length == 0 {
				continue
			}
			if extent.Flags&fiemapExtentShared == 0 {
				privateBytes += int64(extent.Length)
				continue
			}
			sawShared = true
			sharedBytes += sharedExtents.add(extent.Physical, extent.Length)
		}

		last := extents[len(extents)-1]
		if last.Flags&fiemapExtentLast != 0 {
			break
		}
		next := last.Logical + last.Length
		if next <= start {
			break
		}
		start = next
	}

	return privateBytes, sharedBytes, sawShared, nil
}

func (t *sharedExtentTracker) add(start, length uint64) int64 {
	if length == 0 {
		return 0
	}

	end := start + length
	first := sort.Search(len(t.ranges), func(i int) bool {
		return t.ranges[i].end >= start
	})
	last := first
	mergedStart := start
	mergedEnd := end
	added := length
	for last < len(t.ranges) && t.ranges[last].start <= mergedEnd {
		existing := t.ranges[last]
		overlapStart := max(start, existing.start)
		overlapEnd := min(end, existing.end)
		if overlapStart < overlapEnd {
			added -= overlapEnd - overlapStart
		}
		mergedStart = min(mergedStart, existing.start)
		mergedEnd = max(mergedEnd, existing.end)
		last++
	}

	merged := physicalRange{start: mergedStart, end: mergedEnd}
	if first == last {
		t.ranges = append(t.ranges, physicalRange{})
		copy(t.ranges[first+1:], t.ranges[first:])
		t.ranges[first] = merged
	} else {
		t.ranges[first] = merged
		copy(t.ranges[first+1:], t.ranges[last:])
		t.ranges = t.ranges[:len(t.ranges)-(last-first)+1]
	}
	return int64(added)
}
