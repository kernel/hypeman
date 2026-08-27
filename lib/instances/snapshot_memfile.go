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
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

const cloudHypervisorSnapshotMemoryRelPath = "snapshots/snapshot-latest/memory-ranges"

func sharedSnapshotMemoryRelPath(hvType hypervisor.Type) (string, bool) {
	switch hvType {
	case hypervisor.TypeFirecracker:
		return firecrackerSnapshotMemoryRelPath, true
	case hypervisor.TypeCloudHypervisor:
		return cloudHypervisorSnapshotMemoryRelPath, true
	default:
		return "", false
	}
}

func supportsSharedSnapshotMemory(hvType hypervisor.Type) bool {
	_, ok := sharedSnapshotMemoryRelPath(hvType)
	return ok
}

func sharedSnapshotMemoryPathInGuestDir(guestDir string, hvType hypervisor.Type) (string, bool) {
	relPath, ok := sharedSnapshotMemoryRelPath(hvType)
	if !ok {
		return "", false
	}
	return filepath.Join(guestDir, relPath), true
}

// linkForkSnapshotMemory hardlinks the source snapshot memory into the fork so
// sibling restores map the same inode and share the kernel page cache.
// Firecracker and kernel-paged Cloud Hypervisor restores map the file privately;
// ordinary Cloud Hypervisor restores only read it into guest RAM.
func (m *manager) linkForkSnapshotMemory(ctx context.Context, hvType hypervisor.Type, srcGuestDir, dstGuestDir string) error {
	srcMem, ok := sharedSnapshotMemoryPathInGuestDir(srcGuestDir, hvType)
	if !ok {
		return fmt.Errorf("snapshot memory sharing is not supported for hypervisor %s", hvType)
	}
	dstMem, _ := sharedSnapshotMemoryPathInGuestDir(dstGuestDir, hvType)
	if err := os.MkdirAll(filepath.Dir(dstMem), 0755); err != nil {
		return fmt.Errorf("create fork snapshot dir: %w", err)
	}
	err := os.Link(srcMem, dstMem)
	if err == nil {
		return nil
	}
	logger.FromContext(ctx).WarnContext(ctx, "hardlink of fork snapshot memory failed; falling back to copy",
		"hypervisor", hvType, "source", srcMem, "target", dstMem, "error", err)
	m.recordForkMemFileShareFallback(ctx, hvType, linkFallbackReason(err))
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
