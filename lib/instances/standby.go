package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"go.opentelemetry.io/otel/attribute"
)

// StandbyInstance puts an instance in standby state
// Multi-hop orchestration: Running → Paused → Standby
func (m *manager) standbyInstance(
	ctx context.Context,

	id string,
	req StandbyInstanceRequest,
	skipCompression bool,
) (_ *Instance, retErr error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "putting instance in standby", "instance_id", id)

	ctx, span := m.startLifecycleSpan(ctx, "instances.standby",
		attribute.String("instance_id", id),
		attribute.String("operation", "standby"),
	)
	defer func() { finishInstancesSpan(span, retErr) }()

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	ctx = enrichInstancesTrace(ctx, attribute.String("hypervisor", string(stored.HypervisorType)))
	log.DebugContext(ctx, "loaded instance", "instance_id", id, "state", inst.State)

	// 2. Validate state transition (must be Running to start standby flow)
	if inst.State != StateRunning {
		log.ErrorContext(ctx, "invalid state for standby", "instance_id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot standby from state %s", ErrInvalidState, inst.State)
	}

	// 2b. Block standby for vGPU instances (driver limitation - NVIDIA vGPU doesn't support snapshots)
	if inst.GPUMdevUUID != "" || inst.GPUProfile != "" {
		log.ErrorContext(ctx, "standby not supported for vGPU instances", "instance_id", id, "gpu_profile", inst.GPUProfile)
		return nil, fmt.Errorf("%w: standby is not supported for instances with vGPU attached (driver limitation)", ErrInvalidState)
	}

	// Resolve/validate compression policy early so invalid request/config
	// fails before any state transition side effects.
	var compressionPolicy *snapshotstore.SnapshotCompressionConfig
	var compressionDelay time.Duration
	if !skipCompression {
		policy, err := m.resolveStandbyCompressionPolicy(stored, req.Compression)
		if err != nil {
			if !errors.Is(err, ErrInvalidRequest) {
				err = fmt.Errorf("%w: %v", ErrInvalidRequest, err)
			}
			return nil, err
		}
		compressionPolicy = policy
		if compressionPolicy != nil {
			delay, err := m.resolveStandbyCompressionDelay(stored, req.CompressionDelay)
			if err != nil {
				if !errors.Is(err, ErrInvalidRequest) {
					err = fmt.Errorf("%w: %v", ErrInvalidRequest, err)
				}
				return nil, err
			}
			compressionDelay = delay
		}
	}

	// 3. Get network allocation BEFORE killing VMM (while we can still query it)
	// This is needed to delete the TAP device after VMM shuts down
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "instance_id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "instance_id", id, "error", err)
		}
	}

	// 4. Create hypervisor client
	hv, err := m.getHypervisor(inst.SocketPath, stored.HypervisorType)
	if err != nil {
		log.ErrorContext(ctx, "failed to create hypervisor client", "instance_id", id, "error", err)
		return nil, fmt.Errorf("create hypervisor client: %w", err)
	}

	// 5. Check if snapshot is supported
	if !hv.Capabilities().SupportsSnapshot {
		log.ErrorContext(ctx, "hypervisor does not support snapshots", "instance_id", id, "hypervisor", stored.HypervisorType)
		return nil, fmt.Errorf("hypervisor %s does not support standby (snapshots)", stored.HypervisorType)
	}

	// 6. Transition: Running → Paused
	log.DebugContext(ctx, "pausing VM", "instance_id", id)
	pauseCtx, pauseSpanEnd := m.startLifecycleStep(ctx, "pause_vm",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "pause_vm"),
	)
	if err := hv.Pause(pauseCtx); err != nil {
		pauseSpanEnd(err)
		log.ErrorContext(ctx, "failed to pause VM", "instance_id", id, "error", err)
		return nil, fmt.Errorf("pause vm failed: %w", err)
	}
	pauseSpanEnd(nil)

	// 7. Create snapshot
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	retainedBaseDir := m.paths.InstanceSnapshotBase(id)
	reuseSnapshotBase := m.supportsSnapshotBaseReuse(stored.HypervisorType)
	promotedExistingBase := false
	if reuseSnapshotBase {
		var err error
		promotedExistingBase, err = prepareRetainedSnapshotTarget(snapshotDir, retainedBaseDir)
		if err != nil {
			if resumeErr := hv.Resume(ctx); resumeErr != nil {
				log.ErrorContext(ctx, "failed to resume VM after retained snapshot target preparation error", "instance_id", id, "error", resumeErr)
			}
			return nil, fmt.Errorf("prepare retained snapshot target: %w", err)
		}
		// The diff snapshot below writes dirty pages into the mem-file in
		// place; if fanout forks still hardlink its inode, replace it with a
		// private copy first so their memory is never mutated.
		if err := ensureExclusiveSnapshotMemoryOwnership(ctx, snapshotDir); err != nil {
			if resumeErr := hv.Resume(ctx); resumeErr != nil {
				log.ErrorContext(ctx, "failed to resume VM after snapshot memory unshare error", "instance_id", id, "error", resumeErr)
			}
			if promotedExistingBase {
				if rollbackErr := discardPromotedRetainedSnapshotTarget(snapshotDir); rollbackErr != nil {
					log.WarnContext(ctx, "failed to discard promoted snapshot target after unshare error", "instance_id", id, "error", rollbackErr)
				}
			}
			return nil, fmt.Errorf("unshare snapshot memory: %w", err)
		}
	}
	log.DebugContext(ctx, "creating snapshot", "instance_id", id, "snapshot_dir", snapshotDir)
	snapshotCtx, snapshotSpanEnd := m.startLifecycleStep(ctx, "create_snapshot",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "create_snapshot"),
		attribute.Bool("reuse_snapshot_base", reuseSnapshotBase),
	)
	if err := createSnapshot(snapshotCtx, hv, snapshotDir, reuseSnapshotBase); err != nil {
		snapshotSpanEnd(err)
		// Snapshot failed - try to resume VM
		log.ErrorContext(ctx, "snapshot failed, attempting to resume VM", "instance_id", id, "error", err)
		if resumeErr := hv.Resume(ctx); resumeErr != nil {
			log.ErrorContext(ctx, "failed to resume VM after snapshot error", "instance_id", id, "error", resumeErr)
		}
		if promotedExistingBase {
			if rollbackErr := discardPromotedRetainedSnapshotTarget(snapshotDir); rollbackErr != nil {
				log.WarnContext(ctx, "failed to discard promoted snapshot target after snapshot error", "instance_id", id, "error", rollbackErr)
			}
		}
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	snapshotSpanEnd(nil)

	// 8. Stop VMM gracefully (snapshot is complete)
	log.DebugContext(ctx, "shutting down hypervisor", "instance_id", id)
	shutdownCtx, shutdownSpanEnd := m.startLifecycleStep(ctx, "shutdown_hypervisor",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "shutdown_hypervisor"),
	)
	if err := m.shutdownHypervisor(shutdownCtx, &inst); err != nil {
		shutdownSpanEnd(err)
		// The hypervisor may still be running: releasing its TAP or clearing
		// its identity now would orphan a live VMM. The snapshot on disk is
		// harmless and a retried standby redoes it.
		if resumeErr := hv.Resume(ctx); resumeErr != nil {
			log.ErrorContext(ctx, "failed to resume VM after shutdown error", "instance_id", id, "error", resumeErr)
		}
		return nil, fmt.Errorf("shutdown hypervisor: %w", err)
	}
	shutdownSpanEnd(nil)

	// Firecracker vsock sockets can persist across standby/restore if the process
	// exits ungracefully. Remove stale sockets before restore attempts.
	_ = os.Remove(inst.VsockSocket)
	if matches, err := filepath.Glob(inst.VsockSocket + "_*"); err == nil {
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}
	// Standby/restore keeps the same vsock identity, so any pooled guest-agent
	// gRPC connection now points at a dead VM and must be recreated on restore.
	if dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID); err == nil {
		guest.CloseConn(dialer.Key())
	}
	m.closeFirecrackerUFFDSession(ctx, stored)

	// 9. Release network allocation (delete TAP device)
	// TAP devices with explicit Owner/Group fields do NOT auto-delete when VMM exits
	// They must be explicitly deleted
	if inst.NetworkEnabled {
		m.unregisterEgressProxyInstance(ctx, id)
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		releaseNetworkCtx, releaseNetworkSpanEnd := m.startLifecycleStep(ctx, "release_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "release_network"),
		)
		if err := m.networkManager.ReleaseAllocation(releaseNetworkCtx, networkAlloc); err != nil {
			releaseNetworkSpanEnd(err)
			// Log error but continue - snapshot was created successfully
			log.WarnContext(ctx, "failed to release network, continuing with standby", "instance_id", id, "error", err)
		} else {
			releaseNetworkSpanEnd(nil)
		}
	}

	// 10. Update timestamp and clear PID (hypervisor no longer running)
	now := time.Now().UTC()
	stored.StoppedAt = &now
	stored.HypervisorProcessIdentity.Clear()
	stored.PendingStandbyCompression = nil
	clearFirecrackerUFFDRestoreState(stored)
	if err := m.refreshFirecrackerSnapshotCacheKey(stored, snapshotDir); err != nil {
		log.WarnContext(ctx, "failed to refresh firecracker snapshot cache key", "instance_id", id, "error", err)
	}
	if compressionPolicy != nil {
		stored.PendingStandbyCompression = &PendingStandbyCompression{
			Policy:    *cloneCompressionConfig(compressionPolicy),
			NotBefore: m.nowUTC().Add(compressionDelay),
		}
	}
	stored.Phases.Record(phasetracking.PhaseStandby, now)

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDurationWithCompression(ctx, m.metrics.standbyDuration, start, "success", stored.HypervisorType, compressionPolicy)
		m.recordStateTransition(ctx, string(StateRunning), string(StateStandby), stored.HypervisorType)
	}

	// Return instance with derived state (should be Standby now)
	finalInst := m.toInstance(ctx, meta)

	if compressionPolicy != nil {
		log.InfoContext(ctx, "enqueueing standby snapshot compression",
			"instance_id", id,
			"operation", "enqueue_snapshot_compression",
			"source", string(snapshotCompressionSourceStandby),
			"algorithm", string(compressionPolicy.Algorithm),
			"compression_delay", compressionDelay.String(),
		)
		compressionCtx, compressionSpanEnd := m.startLifecycleStep(ctx, "enqueue_snapshot_compression",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "enqueue_snapshot_compression"),
			attribute.Float64("compression_delay_seconds", compressionDelay.Seconds()),
		)
		m.startCompressionJob(compressionCtx, compressionTarget{
			Key:            m.snapshotJobKeyForInstance(stored.Id),
			OwnerID:        stored.Id,
			SnapshotDir:    snapshotDir,
			HypervisorType: stored.HypervisorType,
			Source:         snapshotCompressionSourceStandby,
			Policy:         *compressionPolicy,
			Delay:          compressionDelay,
		})
		compressionSpanEnd(nil)
	}

	log.InfoContext(ctx, "instance put in standby successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}

// createSnapshot creates a snapshot using the hypervisor interface
func createSnapshot(ctx context.Context, hv hypervisor.Hypervisor, snapshotDir string, reuseSnapshotBase bool) error {
	log := logger.FromContext(ctx)

	// Remove old snapshot if the hypervisor does not support reusing snapshots
	// (diff-based snapshots).
	if !reuseSnapshotBase {
		os.RemoveAll(snapshotDir)
	}

	// Create snapshot directory
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	// Create snapshot via hypervisor API
	log.DebugContext(ctx, "invoking hypervisor snapshot API", "snapshot_dir", snapshotDir)
	if err := hv.Snapshot(ctx, snapshotDir); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	log.DebugContext(ctx, "snapshot created successfully", "snapshot_dir", snapshotDir)
	return nil
}

// prepareRetainedSnapshotTarget clears any stale snapshot target from a prior failed
// standby attempt, then moves a retained snapshot base into place when needed.
// The returned bool reports whether an existing retained base was promoted, so callers
// know if they should discard the promoted target on snapshot failure.
func prepareRetainedSnapshotTarget(snapshotDir string, retainedBaseDir string) (bool, error) {
	if _, err := os.Stat(snapshotDir); err == nil {
		if err := os.RemoveAll(snapshotDir); err != nil {
			return false, err
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	if _, err := os.Stat(retainedBaseDir); err == nil {
		if err := os.Rename(retainedBaseDir, snapshotDir); err != nil {
			return false, err
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}

	return false, nil
}

func discardPromotedRetainedSnapshotTarget(snapshotDir string) error {
	return os.RemoveAll(snapshotDir)
}

func restoreRetainedSnapshotBase(snapshotDir string, retainedBaseDir string) error {
	if err := os.RemoveAll(retainedBaseDir); err != nil {
		return err
	}
	if err := os.Rename(snapshotDir, retainedBaseDir); err != nil {
		return err
	}
	return nil
}

// shutdownHypervisor gracefully shuts down the hypervisor process via API
func (m *manager) shutdownHypervisor(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)

	// Resolve the live owner before any teardown: the stored PID may be stale
	// or recycled, and signaling it raw would bypass the ownership checks the
	// kill paths enforce. Failing closed here also keeps the control socket in
	// place as evidence for a later hardened kill.
	pid, err := resolveLiveHypervisorPID(inst.HypervisorProcessIdentity, inst.SocketPath)
	if err != nil {
		return fmt.Errorf("confirm hypervisor ownership before shutdown: %w", err)
	}

	// Try to connect to hypervisor
	hv, err := m.getHypervisor(inst.SocketPath, inst.HypervisorType)
	if err != nil {
		if pid > 0 {
			// The control client cannot be built but the resolved owner is
			// alive; teardown is committed, so kill it rather than report a
			// completed shutdown for a VMM that is still running.
			log.WarnContext(ctx, "could not connect to hypervisor, force killing resolved owner", "instance_id", inst.Id, "pid", pid, "error", err)
			if err := m.terminateThenKill(ctx, inst, pid); err != nil {
				return err
			}
		}
		// The hypervisor is confirmed gone (killed above or no live owner);
		// remove its stale socket.
		_ = os.Remove(inst.SocketPath)
		return nil
	}

	caps := hv.Capabilities()

	// Try graceful shutdown
	shutdownErr := hypervisor.ErrNotSupported
	if !caps.SupportsGracefulVMMShutdown {
		log.DebugContext(ctx, "skipping graceful hypervisor shutdown; hypervisor does not support it", "instance_id", inst.Id)
	} else {
		log.DebugContext(ctx, "sending shutdown command to hypervisor", "instance_id", inst.Id)
		shutdownErr = hv.Shutdown(ctx)
	}

	// Wait for process to exit
	if pid > 0 {
		shouldWaitForGracefulExit := caps.SupportsGracefulVMMShutdown && shutdownErr != hypervisor.ErrNotSupported
		if shouldWaitForGracefulExit {
			if WaitForProcessExit(pid, 2*time.Second) {
				log.DebugContext(ctx, "hypervisor shutdown gracefully", "instance_id", inst.Id, "pid", pid)
			} else {
				log.WarnContext(ctx, "hypervisor did not exit gracefully in time, force killing process", "instance_id", inst.Id, "pid", pid)
				if err := m.terminateThenKill(ctx, inst, pid); err != nil {
					return err
				}
			}
		} else {
			log.DebugContext(ctx, "skipping graceful exit wait; force killing hypervisor process", "instance_id", inst.Id, "pid", pid)
			if err := m.terminateThenKill(ctx, inst, pid); err != nil {
				return err
			}
		}
	}

	// The hypervisor is confirmed gone (graceful exit, force kill, or no live
	// owner), so its socket is stale now. Removing it any earlier would unlink
	// the control socket of a VMM that survives the kill and gets resumed,
	// leaving that VM unreachable for a later graceful standby or stop.
	_ = os.Remove(inst.SocketPath)

	// A graceful-API error at this point is not a failure: an error from this
	// function means the hypervisor may still be running.
	if shutdownErr != nil && shutdownErr != hypervisor.ErrNotSupported {
		log.WarnContext(ctx, "graceful hypervisor shutdown failed, process force killed", "instance_id", inst.Id, "error", shutdownErr)
	}

	return nil
}
