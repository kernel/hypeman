package instances

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/attribute"
)

const deleteGracefulShutdownTimeout = 2

// DeleteInstanceOptions configures instance deletion.
type DeleteInstanceOptions struct {
	SkipGracefulShutdown bool
}

// deleteInstance stops and deletes an instance.
func (m *manager) deleteInstance(ctx context.Context, id string) error {
	return m.deleteInstanceWithOptions(ctx, id, DeleteInstanceOptions{})
}

func (m *manager) deleteInstanceWithOptions(
	ctx context.Context,
	id string,
	options DeleteInstanceOptions,
) (retErr error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "deleting instance", "instance_id", id)

	ctx, span := m.startLifecycleSpan(ctx, "instances.delete",
		attribute.String("instance_id", id),
		attribute.String("operation", "delete"),
	)
	defer func() { finishInstancesSpan(span, retErr) }()

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "instance_id", id, "error", err)
		return err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	ctx = enrichInstancesTrace(ctx,
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("instance_state", string(inst.State)),
	)
	log.DebugContext(ctx, "loaded instance", "instance_id", id, "state", inst.State)

	compressionCtx, compressionSpanEnd := m.startLifecycleStep(ctx, "cancel_compression_job",
		attribute.String("instance_id", id),
		attribute.String("operation", "cancel_compression_job"),
	)
	target, err := m.cancelAndWaitCompressionJob(compressionCtx, m.snapshotJobKeyForInstance(id))
	compressionSpanEnd(err)
	if err != nil {
		return fmt.Errorf("wait for instance compression to stop: %w", err)
	}
	if target != nil && target.State == compressionJobStateRunning {
		m.recordSnapshotCompressionPreemption(ctx, snapshotCompressionPreemptionDeleteInstance, target.Target)
	}

	// 2. Get network allocation BEFORE killing VMM (while we can still query it)
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "instance_id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "instance_id", id, "error", err)
		}
	}

	// 3. Close exec gRPC connection before killing hypervisor to prevent panic
	if dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID); err == nil {
		guest.CloseConn(dialer.Key())
	}

	// 3b. Block the restart policy before any teardown. If the delete fails
	// partway (e.g. the hypervisor cannot be confirmed dead) the metadata is
	// retained with the VMM already stopped, and without this marker the
	// restart policy controller would start the instance again.
	if err := m.markRestartManualStopLocked(ctx, id); err != nil {
		return fmt.Errorf("block restart policy before delete: %w", err)
	}
	// markRestartManualStopLocked persists through a separate metadata load.
	// Reload it so later saves in this delete do not overwrite the block.
	meta, err = m.loadMetadata(id)
	if err != nil {
		return fmt.Errorf("reload metadata after blocking restart policy: %w", err)
	}
	stored = &meta.StoredMetadata

	// 4. If active, try graceful guest shutdown before force kill.
	gracefulShutdown := false
	if !options.SkipGracefulShutdown && (inst.State == StateRunning || inst.State == StateInitializing) {
		stopTimeout := resolveStopTimeout(stored)
		if stopTimeout > deleteGracefulShutdownTimeout {
			stopTimeout = deleteGracefulShutdownTimeout
		}
		gracefulCtx, gracefulSpanEnd := m.startLifecycleStep(ctx, "graceful_guest_shutdown",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "graceful_guest_shutdown"),
		)
		gracefulShutdown = m.tryGracefulGuestShutdown(gracefulCtx, &inst, stopTimeout)
		if gracefulShutdown {
			gracefulSpanEnd(nil)
		} else {
			gracefulSpanEnd(errGracefulShutdownFailed)
			log.DebugContext(ctx, "graceful shutdown before delete did not complete", "instance_id", id)
		}
	}

	// 5. If hypervisor might be running, force kill it
	// Also attempt kill for StateUnknown since we can't be sure if hypervisor is running
	if !gracefulShutdown && (inst.State.RequiresVMM() || inst.State == StateUnknown) {
		log.DebugContext(ctx, "stopping hypervisor", "instance_id", id, "state", inst.State)
		killCtx, killSpanEnd := m.startLifecycleStep(ctx, "force_kill_hypervisor",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "force_kill_hypervisor"),
		)
		err := m.killHypervisor(killCtx, &inst)
		killSpanEnd(err)
		if err != nil {
			// The hypervisor may still be running, so tearing down its vGPU,
			// network, and devices is unsafe. The restart policy is already
			// blocked and the metadata is retained, so a retried delete is safe.
			log.ErrorContext(ctx, "failed to kill hypervisor; retaining instance metadata", "instance_id", id, "error", err)
			return fmt.Errorf("kill hypervisor: %w", err)
		}
	}
	m.closeFirecrackerUFFDSession(ctx, stored)

	// 5b. Release the vGPU assignment if present, before any network, device,
	// or volume teardown. Release failure is logged and the delete continues,
	// matching the pre-refactor contract: the VMM is already confirmed dead,
	// the guards inside the release never destroy a device they cannot prove
	// is unowned, and a skipped release is recovered by startup
	// reconciliation.
	hadVGPUAssignment := storedVGPUDevicePath(stored) != ""
	if hadVGPUAssignment {
		log.InfoContext(ctx, "destroying vGPU", "instance_id", id, "uuid", stored.GPUMdevUUID)
	}
	if err := releaseStoredVGPU(ctx, stored); err != nil {
		// Log error but continue with cleanup.
		log.WarnContext(ctx, "failed to destroy vGPU, continuing with cleanup", "instance_id", id, "uuid", stored.GPUMdevUUID, "error", err)
	} else if hadVGPUAssignment {
		if err := m.saveMetadata(meta); err != nil {
			log.WarnContext(ctx, "failed to save metadata after vGPU release", "instance_id", id, "error", err)
		}
	}

	// 6. Release network allocation
	if inst.NetworkEnabled {
		m.unregisterEgressProxyInstance(ctx, id)
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		releaseNetworkCtx, releaseNetworkSpanEnd := m.startLifecycleStep(ctx, "release_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "release_network"),
		)
		err := m.networkManager.ReleaseAllocation(releaseNetworkCtx, networkAlloc)
		releaseNetworkSpanEnd(err)
		if err != nil {
			// Log error but continue with cleanup
			log.WarnContext(ctx, "failed to release network, continuing with cleanup", "instance_id", id, "error", err)
		}
	}

	// 7. Detach and auto-unbind devices from VFIO
	if len(inst.Devices) > 0 && m.deviceManager != nil {
		for _, deviceID := range inst.Devices {
			log.DebugContext(ctx, "detaching device", "id", id, "device", deviceID)
			// Mark device as detached
			if err := m.deviceManager.MarkDetached(ctx, deviceID); err != nil {
				log.WarnContext(ctx, "failed to mark device as detached", "id", id, "device", deviceID, "error", err)
			}
			// Auto-unbind from VFIO so native driver can reclaim it
			log.InfoContext(ctx, "auto-unbinding device from VFIO", "id", id, "device", deviceID)
			if err := m.deviceManager.UnbindFromVFIO(ctx, deviceID); err != nil {
				// Log but continue - device might already be unbound or in use by another instance
				log.WarnContext(ctx, "failed to unbind device from VFIO", "id", id, "device", deviceID, "error", err)
			}
		}
	}

	// 7b. Detach volumes
	if len(inst.Volumes) > 0 {
		log.DebugContext(ctx, "detaching volumes", "instance_id", id, "count", len(inst.Volumes))
		for _, volAttach := range inst.Volumes {
			if err := m.volumeManager.DetachVolume(ctx, volAttach.VolumeID, id); err != nil {
				// Log error but continue with cleanup
				log.WarnContext(ctx, "failed to detach volume, continuing with cleanup", "instance_id", id, "volume_id", volAttach.VolumeID, "error", err)
			}
		}
	}

	// 8. Delete all instance data
	log.DebugContext(ctx, "deleting instance data", "instance_id", id)
	_, dataSpanEnd := m.startLifecycleStep(ctx, "delete_instance_data",
		attribute.String("instance_id", id),
		attribute.String("operation", "delete_instance_data"),
	)
	if err := m.deleteInstanceData(id); err != nil {
		dataSpanEnd(err)
		log.ErrorContext(ctx, "failed to delete instance data", "instance_id", id, "error", err)
		return fmt.Errorf("delete instance data: %w", err)
	}
	dataSpanEnd(nil)

	log.InfoContext(ctx, "instance deleted successfully", "instance_id", id)
	return nil
}

// killHypervisor force kills the hypervisor process without graceful shutdown.
// Used by delete and as stop's final fallback after graceful shutdown fails.
// It returns an error when the hypervisor may still be running: neither process
// identity nor socket ownership can be confirmed, SIGKILL fails with an error
// other than ESRCH, or the process does not exit after SIGKILL. Callers must not
// tear down instance resources in that case.
func (m *manager) killHypervisor(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)

	pid, err := resolveLiveHypervisorPID(inst.HypervisorProcessIdentity, inst.SocketPath)
	if err != nil {
		return err
	}
	if pid > 0 {
		if inst.HypervisorPID != nil && pid != *inst.HypervisorPID {
			log.WarnContext(ctx, "stored hypervisor PID does not own the instance socket, killing the socket owner",
				"instance_id", inst.Id, "stored_pid", *inst.HypervisorPID, "owner_pid", pid)
		}
		log.DebugContext(ctx, "killing hypervisor process", "instance_id", inst.Id, "pid", pid)
		if err := killProcessAndWait(pid); err != nil {
			return err
		}
	}

	// The hypervisor is confirmed gone; remove its stale socket.
	os.Remove(inst.SocketPath)

	return nil
}

// WaitForProcessExit polls for a process to exit, returns true if exited within timeout.
// Exported for use in tests.
func WaitForProcessExit(pid int, timeout time.Duration) bool {
	if pid <= 0 {
		return true
	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Reap exited child processes to avoid treating zombies as still-running.
		var status syscall.WaitStatus
		reapedPID, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		switch {
		case waitErr == nil && reapedPID == pid:
			return true
		case waitErr == nil && reapedPID == 0:
			// Process still running (or wait status not yet available).
		case waitErr == syscall.ECHILD:
			// Not our child (or already reaped elsewhere). Fall back to existence check.
			if !ProcessExists(pid) {
				return true
			}
		default:
			// Best effort fallback on transient/unexpected wait errors.
			if !ProcessExists(pid) {
				return true
			}
		}

		// Still alive, wait a bit before checking again
		// 10ms polling interval balances responsiveness with CPU usage
		time.Sleep(10 * time.Millisecond)
	}

	// Timeout reached, process still exists
	return false
}
