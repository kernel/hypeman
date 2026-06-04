package instances

import (
	"context"
	"errors"
	"fmt"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
)

func withSnapshotSourceAliasReadLock(run func() error) error {
	unlockAliasReaders := hypervisor.LockSnapshotSourceAliasReaders()
	defer unlockAliasReaders()
	return run()
}

func prepareForkWithAliasReadLock(ctx context.Context, starter hypervisor.VMStarter, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	var result hypervisor.ForkPrepareResult
	err := withSnapshotSourceAliasReadLock(func() error {
		var err error
		result, err = starter.PrepareFork(ctx, req)
		return err
	})
	return result, err
}

func copyGuestDirectoryWithAliasReadLock(srcDir, dstDir string) error {
	return withSnapshotSourceAliasReadLock(func() error {
		return forkvm.CopyGuestDirectory(srcDir, dstDir)
	})
}

func (m *manager) copyForkSourceGuestDirectory(ctx context.Context, sourceState State, sourceID string, stored *StoredMetadata, srcDir, dstDir, deferredSnapshotMemoryPath string) error {
	if sourceState == StateStandby {
		if err := m.ensureSnapshotMemoryReady(ctx, m.paths.InstanceSnapshotLatest(sourceID), m.snapshotJobKeyForInstance(sourceID), stored.HypervisorType); err != nil {
			return fmt.Errorf("prepare standby snapshot for fork: %w", err)
		}
	}
	copyOptions := forkvm.CopyOptions{}
	if deferredSnapshotMemoryPath != "" {
		copyOptions.SkipRelativePaths = map[string]struct{}{firecrackerSnapshotMemoryRelPath: {}}
	}
	return withSnapshotSourceAliasReadLock(func() error {
		if err := forkvm.CopyGuestDirectoryWithOptions(srcDir, dstDir, copyOptions); err != nil {
			if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
				return fmt.Errorf("fork requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
			}
			return fmt.Errorf("clone guest directory: %w", err)
		}
		return nil
	})
}

func (m *manager) copySnapshotGuestDirectoryForFork(ctx context.Context, snapshotID string, hvType hypervisor.Type, dstDir, deferredSnapshotMemoryPath string) error {
	if err := m.ensureSnapshotMemoryReady(ctx, m.paths.SnapshotGuestDir(snapshotID), "", hvType); err != nil {
		return fmt.Errorf("prepare snapshot memory for fork: %w", err)
	}
	copyOptions := forkvm.CopyOptions{}
	if deferredSnapshotMemoryPath != "" {
		copyOptions.SkipRelativePaths = map[string]struct{}{firecrackerSnapshotMemoryRelPath: {}}
	}
	return withSnapshotSourceAliasReadLock(func() error {
		if err := forkvm.CopyGuestDirectoryWithOptions(m.paths.SnapshotGuestDir(snapshotID), dstDir, copyOptions); err != nil {
			if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
				return fmt.Errorf("fork from snapshot requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
			}
			return fmt.Errorf("clone snapshot payload: %w", err)
		}
		return nil
	})
}
