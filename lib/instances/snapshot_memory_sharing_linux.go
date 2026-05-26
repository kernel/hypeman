//go:build linux

package instances

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	snapshotMemoryFsIocFiemap        = 0xC020660B
	snapshotMemoryFiemapFlagSync     = 0x00000001
	snapshotMemoryFiemapExtentLast   = 0x00000001
	snapshotMemoryFiemapExtentShared = 0x00002000
	snapshotMemoryMaxFiemapExtents   = 512
)

type snapshotMemoryFiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type snapshotMemoryFiemap struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
	Extents       [snapshotMemoryMaxFiemapExtents]snapshotMemoryFiemapExtent
}

func inspectSnapshotMemorySharing(path string) (snapshotMemorySharing, error) {
	file, err := os.Open(path)
	if err != nil {
		return snapshotMemorySharing{}, fmt.Errorf("open snapshot memory: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return snapshotMemorySharing{}, fmt.Errorf("stat snapshot memory: %w", err)
	}
	if info.Size() == 0 {
		return snapshotMemorySharing{}, nil
	}

	var out snapshotMemorySharing
	start := uint64(0)
	for {
		mapping := snapshotMemoryFiemap{
			Start:       start,
			Length:      ^uint64(0),
			Flags:       snapshotMemoryFiemapFlagSync,
			ExtentCount: snapshotMemoryMaxFiemapExtents,
		}

		_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), uintptr(snapshotMemoryFsIocFiemap), uintptr(unsafe.Pointer(&mapping)))
		if errno != 0 {
			if isFiemapUnsupported(errno) {
				return snapshotMemorySharing{Unknown: true}, nil
			}
			return snapshotMemorySharing{}, fmt.Errorf("fiemap snapshot memory: %w", errno)
		}
		if mapping.MappedExtents == 0 {
			return out, nil
		}

		last := false
		nextStart := start
		for i := uint32(0); i < mapping.MappedExtents; i++ {
			extent := mapping.Extents[i]
			length := int64(extent.Length)
			if extent.Flags&snapshotMemoryFiemapExtentShared != 0 {
				out.SharedBytes += length
			} else {
				out.PrivateBytes += length
			}
			nextStart = extent.Logical + extent.Length
			if extent.Flags&snapshotMemoryFiemapExtentLast != 0 {
				last = true
			}
		}
		if last || nextStart <= start {
			return out, nil
		}
		start = nextStart
	}
}

func isFiemapUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTTY) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EPERM)
}
