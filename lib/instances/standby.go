package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/trace"
)

// StandbyInstance puts an instance in standby state
// Multi-hop orchestration: Running → Paused → Standby
func (m *manager) standbyInstance(
	ctx context.Context,

	id string,
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "putting instance in standby", "instance_id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "StandbyInstance")
		defer span.End()
	}

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
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
	if err := hv.Pause(ctx); err != nil {
		log.ErrorContext(ctx, "failed to pause VM", "instance_id", id, "error", err)
		return nil, fmt.Errorf("pause vm failed: %w", err)
	}

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
	}
	log.DebugContext(ctx, "creating snapshot", "instance_id", id, "snapshot_dir", snapshotDir)
	if err := createSnapshot(ctx, hv, snapshotDir, reuseSnapshotBase); err != nil {
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

	// 8. Stop VMM gracefully (snapshot is complete)
	log.DebugContext(ctx, "shutting down hypervisor", "instance_id", id)
	if err := m.shutdownHypervisor(ctx, &inst); err != nil {
		// Log but continue - snapshot was created successfully
		log.WarnContext(ctx, "failed to shutdown hypervisor gracefully, snapshot still valid", "instance_id", id, "error", err)
	}

	// Firecracker vsock sockets can persist across standby/restore if the process
	// exits ungracefully. Remove stale sockets before restore attempts.
	_ = os.Remove(inst.VsockSocket)
	if matches, err := filepath.Glob(inst.VsockSocket + "_*"); err == nil {
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}

	// 9. Release network allocation (delete TAP device)
	// TAP devices with explicit Owner/Group fields do NOT auto-delete when VMM exits
	// They must be explicitly deleted
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue - snapshot was created successfully
			log.WarnContext(ctx, "failed to release network, continuing with standby", "instance_id", id, "error", err)
		}
	}

	// 10. Update timestamp and clear PID (hypervisor no longer running)
	now := time.Now()
	stored.StoppedAt = &now
	stored.HypervisorPID = nil

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.standbyDuration, start, "success", stored.HypervisorType)
		m.recordStateTransition(ctx, string(StateRunning), string(StateStandby), stored.HypervisorType)
	}

	// Return instance with derived state (should be Standby now)
	finalInst := m.toInstance(ctx, meta)
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
	defer func() {
		// Clean stale sockets even if graceful shutdown fails.
		_ = os.Remove(inst.SocketPath)
	}()

	// Try to connect to hypervisor
	hv, err := m.getHypervisor(inst.SocketPath, inst.HypervisorType)
	if err != nil {
		// Can't connect - hypervisor might already be stopped
		log.DebugContext(ctx, "could not connect to hypervisor, may already be stopped", "instance_id", inst.Id)
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
	if inst.HypervisorPID != nil {
		pid := *inst.HypervisorPID
		shouldWaitForGracefulExit := caps.SupportsGracefulVMMShutdown && shutdownErr != hypervisor.ErrNotSupported
		if shouldWaitForGracefulExit {
			if WaitForProcessExit(pid, 2*time.Second) {
				log.DebugContext(ctx, "hypervisor shutdown gracefully", "instance_id", inst.Id, "pid", pid)
			} else {
				log.WarnContext(ctx, "hypervisor did not exit gracefully in time, force killing process", "instance_id", inst.Id, "pid", pid)
				if err := forceKillHypervisorPID(pid); err != nil {
					return err
				}
			}
		} else {
			log.DebugContext(ctx, "skipping graceful exit wait; force killing hypervisor process", "instance_id", inst.Id, "pid", pid)
			if err := forceKillHypervisorPID(pid); err != nil {
				return err
			}
		}
	}

	if shutdownErr != nil && shutdownErr != hypervisor.ErrNotSupported {
		return fmt.Errorf("graceful hypervisor shutdown failed: %w", shutdownErr)
	}

	return nil
}

func forceKillHypervisorPID(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("force kill hypervisor pid %d: %w", pid, err)
	}
	if WaitForProcessExit(pid, 2*time.Second) {
		return nil
	}

	// The process may have spawned children in its own process group.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if !WaitForProcessExit(pid, 2*time.Second) {
		return fmt.Errorf("hypervisor pid %d did not exit after SIGKILL", pid)
	}
	return nil
}
