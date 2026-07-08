package instances

import (
	"context"
	"errors"
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
func (m *manager) linkForkFirecrackerMemFile(ctx context.Context, srcGuestDir, dstGuestDir string) error {
	srcMem := firecrackerSnapshotMemoryPathInGuestDir(srcGuestDir)
	dstMem := firecrackerSnapshotMemoryPathInGuestDir(dstGuestDir)
	if err := os.MkdirAll(filepath.Dir(dstMem), 0755); err != nil {
		return fmt.Errorf("create fork snapshot dir: %w", err)
	}
	err := os.Link(srcMem, dstMem)
	if err == nil {
		return nil
	}
	// A fallback copy loses the zero-copy fanout and the shared page cache
	// across sibling forks; it should never happen on a correctly provisioned
	// host, so make it visible.
	logger.FromContext(ctx).WarnContext(ctx, "hardlink of fork snapshot memory failed; falling back to copy",
		"source", srcMem, "target", dstMem, "error", err)
	m.recordForkMemFileShareFallback(ctx, linkFallbackReason(err))
	if err := forkvm.CopyRegularFile(srcMem, dstMem); err != nil {
		return fmt.Errorf("copy fork snapshot memory: %w", err)
	}
	return nil
}

func linkFallbackReason(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "unknown"
}

// ensureExclusiveSnapshotMemoryOwnership replaces the snapshot mem-file with a
// private copy when other hardlinks to its inode exist (fanout forks).
// Firecracker merges diff snapshots by writing dirty pages into this file in
// place, which must never mutate memory another instance still reads.
//
// The stat -> copy -> rename sequence is not internally synchronized; callers
// must hold the instance's write lock (the standby path does) so no fork can
// take a new hardlink between the link-count check and the replacement.
func ensureExclusiveSnapshotMemoryOwnership(ctx context.Context, snapshotDir string) error {
	memPath := filepath.Join(snapshotDir, "memory")
	tmpPath := memPath + ".unshare.tmp"
	// Sweep any stale tmp from a crash between copy and rename; it is
	// mem-file-sized and must not linger.
	_ = os.Remove(tmpPath)

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
