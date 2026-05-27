package diskutilization

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kernel/hypeman/lib/paths"
)

const (
	ComponentImages               = "images"
	ComponentOCICache             = "oci_cache"
	ComponentVolumes              = "volumes"
	ComponentRootfsOverlays       = "rootfs_overlays"
	ComponentVolumeOverlays       = "volume_overlays"
	ComponentSnapshotUncompressed = "snapshot_uncompressed"
	ComponentSnapshotCompressed   = "snapshot_compressed"
	ComponentSnapshotShared       = "snapshot_shared"
	ComponentSnapshotOther        = "snapshot_other"
)

type Breakdown struct {
	Images               int64
	OCICache             int64
	Volumes              int64
	RootfsOverlays       int64
	VolumeOverlays       int64
	SnapshotUncompressed int64
	SnapshotCompressed   int64
	SnapshotShared       int64
	SnapshotOther        int64
}

func (b Breakdown) Components() map[string]int64 {
	return map[string]int64{
		ComponentImages:               b.Images,
		ComponentOCICache:             b.OCICache,
		ComponentVolumes:              b.Volumes,
		ComponentRootfsOverlays:       b.RootfsOverlays,
		ComponentVolumeOverlays:       b.VolumeOverlays,
		ComponentSnapshotUncompressed: b.SnapshotUncompressed,
		ComponentSnapshotCompressed:   b.SnapshotCompressed,
		ComponentSnapshotShared:       b.SnapshotShared,
		ComponentSnapshotOther:        b.SnapshotOther,
	}
}

func Collect(p *paths.Paths) (Breakdown, error) {
	var breakdown Breakdown

	var err error
	breakdown.Images, err = sumMatchingFilesAllocatedBytes(p.ImagesDir(), func(path string, entry fs.DirEntry) bool {
		if entry.IsDir() {
			return false
		}
		name := entry.Name()
		return name == "rootfs.erofs" || name == "rootfs.ext4"
	})
	if err != nil {
		return Breakdown{}, err
	}

	breakdown.OCICache, err = sumTreeAllocatedBytes(p.SystemOCICache())
	if err != nil {
		return Breakdown{}, err
	}

	breakdown.Volumes, err = sumDirectChildFileAllocatedBytes(p.VolumesDir(), "data.raw")
	if err != nil {
		return Breakdown{}, err
	}

	guestEntries, err := os.ReadDir(p.GuestsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return breakdown, nil
		}
		return Breakdown{}, err
	}

	sharedSnapshotExtents := newSharedExtentTracker()
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
			return Breakdown{}, err
		}
		breakdown.VolumeOverlays += volumeOverlays

		snapshotDir := p.InstanceSnapshotLatest(instanceID)
		classification, exists, err := classifySnapshotDir(snapshotDir)
		if err != nil {
			return Breakdown{}, err
		}
		if !exists {
			continue
		}

		snapshotBytes, sharedSnapshotBytes, err := sumSnapshotTreeAllocatedBytes(snapshotDir, sharedSnapshotExtents)
		if err != nil {
			return Breakdown{}, err
		}
		breakdown.SnapshotShared += sharedSnapshotBytes

		switch classification {
		case ComponentSnapshotCompressed:
			breakdown.SnapshotCompressed += snapshotBytes
		case ComponentSnapshotUncompressed:
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
		return ComponentSnapshotOther, true, nil
	}

	switch {
	case pathExists(filepath.Join(snapshotDir, "memory-ranges.zst")):
		return ComponentSnapshotCompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory.zst")):
		return ComponentSnapshotCompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory-ranges.lz4")):
		return ComponentSnapshotCompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory.lz4")):
		return ComponentSnapshotCompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory-ranges")):
		return ComponentSnapshotUncompressed, true, nil
	case pathExists(filepath.Join(snapshotDir, "memory")):
		return ComponentSnapshotUncompressed, true, nil
	default:
		return ComponentSnapshotOther, true, nil
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

func sumSnapshotTreeAllocatedBytes(root string, sharedExtents *sharedExtentTracker) (int64, int64, error) {
	var privateTotal int64
	var sharedTotal int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		privateBytes, sharedBytes := snapshotAllocatedBytesForPath(path, sharedExtents)
		privateTotal += privateBytes
		sharedTotal += sharedBytes
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	return privateTotal, sharedTotal, nil
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
