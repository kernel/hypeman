package instances

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/kernel/hypeman/lib/paths"
)

func volumeOverlayReservationBytes(volumes []VolumeAttachment) int64 {
	var total int64
	for _, vol := range volumes {
		if vol.Overlay {
			total += vol.OverlaySize
		}
	}
	return total
}

func requestedDiskReservationBytes(overlaySize int64, volumes []VolumeAttachment) int64 {
	return overlaySize + volumeOverlayReservationBytes(volumes)
}

func storedDiskReservationBytes(stored *StoredMetadata) int64 {
	if stored == nil {
		return 0
	}
	if stored.MacOS != nil {
		return macOSBootDiskReservationBytes(stored)
	}
	return requestedDiskReservationBytes(stored.OverlaySize, stored.Volumes)
}

// macOSBootDiskAllowance is the disk reserved for a macOS guest's writes. Its boot
// disk starts as an empty ASIF overlay on layers its image already holds, and grows
// with the guest's writes, with no fixed cap. The allowance matches the Linux
// default overlay size.
const macOSBootDiskAllowance = 10 << 30

// macOSBootDiskReservationBytes reserves the allowance, or what the guest's boot
// overlay already holds once it has written more.
func macOSBootDiskReservationBytes(stored *StoredMetadata) int64 {
	var allocated int64
	if info, err := os.Stat(filepath.Join(stored.DataDir, paths.MacOSBootDiskFile)); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			allocated = stat.Blocks * 512
		}
	}
	return max(macOSBootDiskAllowance, allocated)
}
