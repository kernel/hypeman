package instances

import (
	"context"
	"errors"
	"fmt"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func withSnapshotSourceAliasReadLock(run func() error) error {
	unlockAliasReaders := hypervisor.LockSnapshotSourceAliasReaders()
	defer unlockAliasReaders()
	return run()
}

func prepareForkWithAliasReadLock(ctx context.Context, starter hypervisor.VMStarter, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	ctx, span := startInstancesSpan(ctx, otel.Tracer("hypeman/instances"), "instances.snapshot_alias.prepare_fork",
		attribute.String("operation", "snapshot_alias_prepare_fork"),
	)
	var retErr error
	defer func() { finishInstancesSpan(span, retErr) }()

	var result hypervisor.ForkPrepareResult
	retErr = withSnapshotSourceAliasReadLock(func() error {
		var err error
		result, err = starter.PrepareFork(ctx, req)
		return err
	})
	return result, retErr
}

func copyGuestDirectoryWithAliasReadLock(srcDir, dstDir string) error {
	return withSnapshotSourceAliasReadLock(func() error {
		return forkvm.CopyGuestDirectory(srcDir, dstDir)
	})
}

func (m *manager) copyForkSourceGuestDirectory(ctx context.Context, sourceState State, sourceID string, stored *StoredMetadata, srcDir, dstDir, deferredSnapshotMemoryPath string) error {
	ctx, span := m.tracerOrDefault().Start(ctx, "instances.fork.copy_guest_directory",
		trace.WithAttributes(
			attribute.String("operation", "fork_copy_guest_directory"),
			attribute.String("instance_id", sourceID),
			attribute.String("source_state", string(sourceState)),
			attribute.Bool("deferred_snapshot_memory", deferredSnapshotMemoryPath != ""),
		),
	)
	var retErr error
	defer func() { finishInstancesSpan(span, retErr) }()

	if sourceState == StateStandby {
		readyCtx, readyDone := m.startLifecycleStep(ctx, "instances.fork.copy_guest_directory.ensure_snapshot_memory_ready",
			attribute.String("operation", "fork_copy_ensure_snapshot_memory_ready"),
			attribute.String("instance_id", sourceID),
			attribute.String("hypervisor", string(stored.HypervisorType)),
		)
		if err := m.ensureSnapshotMemoryReady(readyCtx, m.paths.InstanceSnapshotLatest(sourceID), m.snapshotJobKeyForInstance(sourceID), stored.HypervisorType); err != nil {
			readyDone(err)
			retErr = fmt.Errorf("prepare standby snapshot for fork: %w", err)
			return retErr
		}
		readyDone(nil)
	}

	copyOptions := forkvm.CopyOptions{}
	if deferredSnapshotMemoryPath != "" {
		copyOptions.SkipRelativePaths = map[string]struct{}{firecrackerSnapshotMemoryRelPath: {}}
	}

	_, cloneDone := m.startLifecycleStep(ctx, "instances.fork.copy_guest_directory.clone",
		attribute.String("operation", "fork_copy_guest_directory_clone"),
		attribute.String("instance_id", sourceID),
		attribute.Bool("deferred_snapshot_memory", deferredSnapshotMemoryPath != ""),
	)
	retErr = withSnapshotSourceAliasReadLock(func() error {
		if err := forkvm.CopyGuestDirectoryWithOptions(srcDir, dstDir, copyOptions); err != nil {
			if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
				return fmt.Errorf("fork requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
			}
			return fmt.Errorf("clone guest directory: %w", err)
		}
		return nil
	})
	cloneDone(retErr)
	return retErr
}

func (m *manager) copySnapshotGuestDirectoryForFork(ctx context.Context, snapshotID string, hvType hypervisor.Type, dstDir, deferredSnapshotMemoryPath string) error {
	ctx, span := m.tracerOrDefault().Start(ctx, "instances.snapshot.copy_guest_directory",
		trace.WithAttributes(
			attribute.String("operation", "snapshot_copy_guest_directory"),
			attribute.String("snapshot_id", snapshotID),
			attribute.String("hypervisor", string(hvType)),
			attribute.Bool("deferred_snapshot_memory", deferredSnapshotMemoryPath != ""),
		),
	)
	var retErr error
	defer func() { finishInstancesSpan(span, retErr) }()

	readyCtx, readyDone := m.startLifecycleStep(ctx, "instances.snapshot.copy_guest_directory.ensure_snapshot_memory_ready",
		attribute.String("operation", "snapshot_copy_ensure_snapshot_memory_ready"),
		attribute.String("snapshot_id", snapshotID),
		attribute.String("hypervisor", string(hvType)),
	)
	if err := m.ensureSnapshotMemoryReady(readyCtx, m.paths.SnapshotGuestDir(snapshotID), "", hvType); err != nil {
		readyDone(err)
		retErr = fmt.Errorf("prepare snapshot memory for fork: %w", err)
		return retErr
	}
	readyDone(nil)

	copyOptions := forkvm.CopyOptions{}
	if deferredSnapshotMemoryPath != "" {
		copyOptions.SkipRelativePaths = map[string]struct{}{firecrackerSnapshotMemoryRelPath: {}}
	}

	_, cloneDone := m.startLifecycleStep(ctx, "instances.snapshot.copy_guest_directory.clone",
		attribute.String("operation", "snapshot_copy_guest_directory_clone"),
		attribute.String("snapshot_id", snapshotID),
		attribute.Bool("deferred_snapshot_memory", deferredSnapshotMemoryPath != ""),
	)
	retErr = withSnapshotSourceAliasReadLock(func() error {
		if err := forkvm.CopyGuestDirectoryWithOptions(m.paths.SnapshotGuestDir(snapshotID), dstDir, copyOptions); err != nil {
			if errors.Is(err, forkvm.ErrSparseCopyUnsupported) {
				return fmt.Errorf("fork from snapshot requires sparse-capable filesystem (SEEK_DATA/SEEK_HOLE unsupported): %w", err)
			}
			return fmt.Errorf("clone snapshot payload: %w", err)
		}
		return nil
	})
	cloneDone(retErr)
	return retErr
}
