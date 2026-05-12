package instances

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	templateSharedMemFileName    = "memory"
	templateSharedMemFileRelPath = "snapshots/snapshot-latest/memory"
)

// installForkSharedMemFile arranges the fork's snapshot directory so the
// guest mem-file is a hardlink to the source template instance's snapshot
// mem-file instead of a per-fork copy. firecracker mmaps the mem-file
// MAP_PRIVATE during restore, so all forks COW from the same backing inode.
//
// Layout: forkDataDir is the fork's data dir. The snapshot dir is at
// <forkDataDir>/snapshots/snapshot-latest, and the mem-file lives at
// <snapshot dir>/memory. The hardlink shares the inode with the source
// instance's standby snapshot mem-file.
//
// We use a hardlink rather than a symlink because firecracker's restore
// path temporarily aliases the source data dir to the fork data dir while
// it loads the snapshot (see withSnapshotSourceDirAlias). A symlink whose
// target traverses the source dir would resolve back into the fork dir
// during that window and trip ELOOP; a hardlink resolves by inode so the
// alias has no effect on it. Hardlinks require both paths on the same
// filesystem, which holds for our standard data-dir layout.
func (m *manager) installForkSharedMemFile(forkDataDir, sourceInstanceID string) error {
	srcMem := filepath.Join(m.paths.InstanceSnapshotLatest(sourceInstanceID), templateSharedMemFileName)
	if _, err := os.Stat(srcMem); err != nil {
		return fmt.Errorf("stat template mem-file: %w", err)
	}
	dstSnapshotDir := filepath.Join(forkDataDir, "snapshots", "snapshot-latest")
	if err := os.MkdirAll(dstSnapshotDir, 0o755); err != nil {
		return fmt.Errorf("ensure fork snapshot dir: %w", err)
	}
	dstMem := filepath.Join(dstSnapshotDir, templateSharedMemFileName)
	_ = os.Remove(dstMem)
	if err := os.Link(srcMem, dstMem); err != nil {
		return fmt.Errorf("hardlink shared mem-file: %w", err)
	}
	return nil
}
