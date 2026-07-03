package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/logger"
)

// linkForkFirecrackerMemFile hardlinks the source's snapshot mem-file into the
// fork's guest dir so all forks of a snapshot share one inode: fanout costs no
// copy I/O, and the pager's backing reads hit the kernel page cache warmed by
// sibling forks. Falls back to a reflink/sparse copy when linking fails.
// Sharing an inode is safe because Firecracker mmaps the mem-file MAP_PRIVATE
// and the only file writer, the standby diff snapshot, unshares first via
// ensureExclusiveSnapshotMemoryOwnership.
func linkForkFirecrackerMemFile(ctx context.Context, srcGuestDir, dstGuestDir string) error {
	srcMem := firecrackerSnapshotMemoryPathInGuestDir(srcGuestDir)
	dstMem := firecrackerSnapshotMemoryPathInGuestDir(dstGuestDir)
	if err := os.MkdirAll(filepath.Dir(dstMem), 0755); err != nil {
		return fmt.Errorf("create fork snapshot dir: %w", err)
	}
	err := os.Link(srcMem, dstMem)
	if err == nil {
		return nil
	}
	logger.FromContext(ctx).WarnContext(ctx, "hardlink of fork snapshot memory failed; falling back to copy",
		"source", srcMem, "target", dstMem, "error", err)
	if err := forkvm.CopyRegularFile(srcMem, dstMem); err != nil {
		return fmt.Errorf("copy fork snapshot memory: %w", err)
	}
	return nil
}

// ensureExclusiveSnapshotMemoryOwnership replaces the snapshot mem-file with a
// private copy when other hardlinks to its inode exist (fanout forks).
// Firecracker merges diff snapshots by writing dirty pages into this file in
// place, which must never mutate memory another instance still reads.
func ensureExclusiveSnapshotMemoryOwnership(ctx context.Context, snapshotDir string) error {
	memPath := filepath.Join(snapshotDir, "memory")
	info, err := os.Stat(memPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat snapshot memory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink <= 1 {
		return nil
	}

	start := time.Now()
	tmpPath := memPath + ".unshare.tmp"
	_ = os.Remove(tmpPath)
	if err := forkvm.CopyRegularFile(memPath, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("copy shared snapshot memory: %w", err)
	}
	if err := os.Rename(tmpPath, memPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace shared snapshot memory: %w", err)
	}
	logger.FromContext(ctx).InfoContext(ctx, "unshared snapshot memory before diff snapshot",
		"path", memPath, "links", stat.Nlink, "duration", time.Since(start).String())
	return nil
}
