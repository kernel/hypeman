package resources

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kernel/hypeman/lib/paths"
)

const (
	diskUtilizationComponentImages               = "images"
	diskUtilizationComponentOCICache             = "oci_cache"
	diskUtilizationComponentVolumes              = "volumes"
	diskUtilizationComponentRootfsOverlays       = "rootfs_overlays"
	diskUtilizationComponentVolumeOverlays       = "volume_overlays"
	diskUtilizationComponentSnapshotUncompressed = "snapshot_uncompressed"
	diskUtilizationComponentSnapshotCompressed   = "snapshot_compressed"
	diskUtilizationComponentSnapshotOther        = "snapshot_other"
)

type diskUtilizationBreakdown struct {
	Images               int64
	OCICache             int64
	Volumes              int64
	RootfsOverlays       int64
	VolumeOverlays       int64
	SnapshotUncompressed int64
	SnapshotCompressed   int64
	SnapshotOther        int64
}

func (d diskUtilizationBreakdown) components() map[string]int64 {
	return map[string]int64{
		diskUtilizationComponentImages:               d.Images,
		diskUtilizationComponentOCICache:             d.OCICache,
		diskUtilizationComponentVolumes:              d.Volumes,
		diskUtilizationComponentRootfsOverlays:       d.RootfsOverlays,
		diskUtilizationComponentVolumeOverlays:       d.VolumeOverlays,
		diskUtilizationComponentSnapshotUncompressed: d.SnapshotUncompressed,
		diskUtilizationComponentSnapshotCompressed:   d.SnapshotCompressed,
		diskUtilizationComponentSnapshotOther:        d.SnapshotOther,
	}
}

func collectDiskUtilization(p *paths.Paths) (diskUtilizationBreakdown, error) {
	var breakdown diskUtilizationBreakdown

	var err error
	breakdown.Images, err = sumMatchingFilesAllocatedBytes(p.ImagesDir(), func(path string, entry fs.DirEntry) bool {
		if entry.IsDir() {
			return false
		}
		name := entry.Name()
		return name == "rootfs.erofs" || name == "rootfs.ext4"
	})
	if err != nil {
		return diskUtilizationBreakdown{}, err
	}

	breakdown.OCICache, err = sumTreeAllocatedBytes(p.SystemOCICache())
	if err != nil {
		return diskUtilizationBreakdown{}, err
	}

	breakdown.Volumes, err = sumDirectChildFileAllocatedBytes(p.VolumesDir(), "data.raw")
	if err != nil {
		return diskUtilizationBreakdown{}, err
	}

	guestEntries, err := os.ReadDir(p.GuestsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return breakdown, nil
		}
		return diskUtilizationBreakdown{}, err
	}

	for _, guest := range guestEntries {
		if !guest.IsDir() {
			continue
		}

		instanceID := guest.Name()

		breakdown.RootfsOverlays += allocatedBytesForPath(p.InstanceOverlay(instanceID))

		volumeOverlays, err := sumMatchingFilesAllocatedBytes(p.InstanceVolumeOverlaysDir(instanceID), func(path string, entry fs.DirEntry) bool {
			return !entry.IsDir() && filepath.Ext(entry.Name()) == ".raw"
		})
		if err != nil {
			return diskUtilizationBreakdown{}, err
		}
		breakdown.VolumeOverlays += volumeOverlays

		snapshotDir := p.InstanceSnapshotLatest(instanceID)
		classification, exists, err := classifySnapshotDir(snapshotDir)
		if err != nil {
			return diskUtilizationBreakdown{}, err
		}
		if !exists {
			continue
		}

		snapshotBytes, err := sumTreeAllocatedBytes(snapshotDir)
		if err != nil {
			return diskUtilizationBreakdown{}, err
		}

		switch classification {
		case diskUtilizationComponentSnapshotCompressed:
			breakdown.SnapshotCompressed += snapshotBytes
		case diskUtilizationComponentSnapshotUncompressed:
			breakdown.SnapshotUncompressed += snapshotBytes
		default:
			breakdown.SnapshotOther += snapshotBytes
		}
	}

	return breakdown, nil
}

func classifySnapshotDir(snapshotDir string) (component string, exists bool, err error) {
	info, err := os.Stat(snapshotDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !info.IsDir() {
		return diskUtilizationComponentSnapshotOther, true, nil
	}

	switch {
	case pathExists(filepath.Join(snapshotDir, "memory-ranges.zst")):
		return diskUtilizationComponentSnapshotCompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory-ranges.lz4")):
		return diskUtilizationComponentSnapshotCompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory-ranges")):
		return diskUtilizationComponentSnapshotUncompressed, true, nil
	default:
		return diskUtilizationComponentSnapshotOther, true, nil
	}
}

func sumDirectChildFileAllocatedBytes(root string, childFile string) (int64, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var total int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		total += allocatedBytesForPath(filepath.Join(root, entry.Name(), childFile))
	}

	return total, nil
}

func sumMatchingFilesAllocatedBytes(root string, match func(path string, entry fs.DirEntry) bool) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if match(path, entry) {
			total += allocatedBytesForPath(path)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return total, nil
}

func sumTreeAllocatedBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += allocatedBytesForPath(path)
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return total, nil
}

func allocatedBytesForPath(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}

	return stat.Blocks * 512
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
