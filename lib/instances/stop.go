package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/attribute"
)

// DefaultStopTimeout is the default grace period for graceful shutdown (seconds).
const DefaultStopTimeout = 5
const shutdownRPCDeadline = 1500 * time.Millisecond
const shutdownFailureFallbackWait = 500 * time.Millisecond

var errGracefulShutdownFailed = errors.New("graceful guest shutdown did not complete")

// resolveStopTimeout returns the configured stop timeout in seconds,
// falling back to the package default when unset/invalid.
func resolveStopTimeout(stored *StoredMetadata) int {
	stopTimeout := stored.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = DefaultStopTimeout
	}
	return stopTimeout
}

// tryGracefulGuestShutdown asks guest init to shut down and waits for the
// hypervisor process to exit. Returns true if the process exited in time.
func (m *manager) tryGracefulGuestShutdown(ctx context.Context, inst *Instance, stopTimeout int) bool {
	log := logger.FromContext(ctx)

	if inst.SkipGuestAgent {
		log.DebugContext(ctx, "guest-agent disabled, skipping graceful guest shutdown", "instance_id", inst.Id)
		return false
	}

	log.DebugContext(ctx, "sending graceful shutdown signal to guest", "instance_id", inst.Id, "timeout_seconds", stopTimeout)
	dialer, dialerErr := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if dialerErr != nil {
		log.WarnContext(ctx, "could not create vsock dialer for graceful shutdown", "instance_id", inst.Id, "error", dialerErr)
		return false
	}

	sendShutdown := func() error {
		shutdownCtx, cancel := context.WithTimeout(ctx, shutdownRPCDeadline)
		defer cancel()
		return guest.ShutdownInstance(shutdownCtx, dialer, 0)
	}

	shutdownSent := false
	if err := sendShutdown(); err != nil {
		// Drop potentially stale pooled connection and retry once.
		guest.CloseConn(dialer.Key())
		if retryErr := sendShutdown(); retryErr != nil {
			log.WarnContext(ctx, "shutdown RPC failed; falling back to hypervisor shutdown", "instance_id", inst.Id, "error", retryErr)
		} else {
			shutdownSent = true
		}
	} else {
		shutdownSent = true
	}

	// Wait for the hypervisor process to exit (init calls reboot(POWER_OFF))
	if inst.HypervisorPID != nil {
		waitTimeout := time.Duration(stopTimeout) * time.Second
		if !shutdownSent && waitTimeout > shutdownFailureFallbackWait {
			// If we couldn't signal the guest, don't burn the full graceful timeout.
			waitTimeout = shutdownFailureFallbackWait
		}

		if WaitForProcessExit(*inst.HypervisorPID, waitTimeout) {
			log.DebugContext(ctx, "VM shut down gracefully", "instance_id", inst.Id)
			return true
		}

		log.WarnContext(ctx, "graceful shutdown timed out, falling back to hypervisor shutdown", "instance_id", inst.Id)
		return false
	}

	return false
}

// forceKillHypervisorProcess sends SIGKILL to the hypervisor process if it's still running
// and waits briefly for it to exit.
func (m *manager) forceKillHypervisorProcess(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)

	if inst.HypervisorPID == nil && inst.SocketPath == "" {
		return nil
	}
	pid, err := resolveLiveHypervisorPID(inst.HypervisorPID, inst.HypervisorStartTime, inst.SocketPath)
	if err != nil {
		return err
	}
	if pid == 0 {
		return nil
	}

	log.WarnContext(ctx, "hypervisor still running after shutdown fallback, sending SIGKILL", "instance_id", inst.Id, "pid", pid)
	if err := sendSIGKILL(pid); err != nil {
		return fmt.Errorf("sigkill hypervisor pid %d: %w", pid, err)
	}
	if !WaitForProcessExit(pid, 30*time.Second) {
		return fmt.Errorf("hypervisor pid %d still alive after SIGKILL", pid)
	}

	log.DebugContext(ctx, "hypervisor process force-killed", "instance_id", inst.Id, "pid", pid)
	return nil
}

func sendSIGKILL(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// stopInstance gracefully stops an active instance.
// Flow: send Shutdown RPC -> wait for VM to power off ->
// fall back to hypervisor shutdown -> final SIGKILL if still alive.
// Multi-hop orchestration: Running/Initializing → Shutdown → Stopped
func (m *manager) stopInstance(
	ctx context.Context,
	id string,
) (_ *Instance, retErr error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "stopping instance", "instance_id", id)

	ctx, span := m.startLifecycleSpan(ctx, "instances.stop",
		attribute.String("instance_id", id),
		attribute.String("operation", "stop"),
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

	// 2. Validate state transition (must be active to stop)
	if inst.State != StateRunning && inst.State != StateInitializing {
		log.ErrorContext(ctx, "invalid state for stop", "instance_id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot stop from state %s, must be Running or Initializing", ErrInvalidState, inst.State)
	}

	// 3. Get network allocation BEFORE killing VMM (while we can still query it)
	var networkAlloc *network.Allocation
	var networkAllocErr error
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "instance_id", id)
		networkAlloc, networkAllocErr = m.networkManager.GetAllocation(ctx, id)
		if networkAllocErr != nil {
			log.WarnContext(ctx, "failed to get network allocation, will fall back to ID-based TAP cleanup", "instance_id", id, "error", networkAllocErr)
		}
	}

	// 4. Graceful shutdown: send signal to guest init via Shutdown RPC,
	// then wait for VM to power off cleanly. Fall back to hypervisor shutdown on timeout.
	stopTimeout := resolveStopTimeout(stored)
	gracefulCtx, gracefulSpanEnd := m.startLifecycleStep(ctx, "graceful_guest_shutdown",
		attribute.String("instance_id", id),
		attribute.String("hypervisor", string(stored.HypervisorType)),
		attribute.String("operation", "graceful_guest_shutdown"),
	)
	gracefulShutdown := m.tryGracefulGuestShutdown(gracefulCtx, &inst, stopTimeout)
	if gracefulShutdown {
		gracefulSpanEnd(nil)
	} else {
		gracefulSpanEnd(errGracefulShutdownFailed)
	}

	// 5. Fallback hypervisor shutdown if guest graceful shutdown didn't work
	if !gracefulShutdown {
		log.DebugContext(ctx, "shutting down hypervisor (fallback)", "instance_id", id)
		shutdownCtx, shutdownSpanEnd := m.startLifecycleStep(ctx, "shutdown_hypervisor",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "shutdown_hypervisor"),
		)
		if err := m.shutdownHypervisor(shutdownCtx, &inst); err != nil {
			shutdownSpanEnd(err)
			// Continue to final SIGKILL fallback if graceful shutdown API fails.
			log.WarnContext(ctx, "failed to shutdown hypervisor", "instance_id", id, "error", err)
		} else {
			shutdownSpanEnd(nil)
		}

		// Final fallback: force-kill the process if it's still alive.
		killCtx, killSpanEnd := m.startLifecycleStep(ctx, "force_kill_hypervisor",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "force_kill_hypervisor"),
		)
		if err := m.forceKillHypervisorProcess(killCtx, &inst); err != nil {
			killSpanEnd(err)
			log.ErrorContext(ctx, "failed to force-kill hypervisor process", "instance_id", id, "error", err)
			return nil, err
		}
		killSpanEnd(nil)
	}

	// 6. Release network allocation (delete TAP device)
	if inst.NetworkEnabled {
		m.unregisterEgressProxyInstance(ctx, id)
	}
	if inst.NetworkEnabled && networkAlloc != nil {
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		releaseNetworkCtx, releaseNetworkSpanEnd := m.startLifecycleStep(ctx, "release_network",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "release_network"),
		)
		if err := m.networkManager.ReleaseAllocation(releaseNetworkCtx, networkAlloc); err != nil {
			releaseNetworkSpanEnd(err)
			// Log error but continue
			log.WarnContext(ctx, "failed to release network, continuing", "instance_id", id, "error", err)
		} else {
			releaseNetworkSpanEnd(nil)
		}
	} else if inst.NetworkEnabled && networkAllocErr != nil {
		// GetAllocation failed earlier, so we don't have a full Allocation. Fall back
		// to deleting the TAP by deterministic name to avoid leaking it on the host.
		log.DebugContext(ctx, "releasing network by instance id (fallback)", "instance_id", id)
		releaseNetworkCtx, releaseNetworkSpanEnd := m.startLifecycleStep(ctx, "release_network_fallback",
			attribute.String("instance_id", id),
			attribute.String("hypervisor", string(stored.HypervisorType)),
			attribute.String("operation", "release_network_fallback"),
		)
		if err := m.networkManager.ReleaseByInstanceID(releaseNetworkCtx, id); err != nil {
			releaseNetworkSpanEnd(err)
			log.WarnContext(ctx, "failed to release network by id, continuing", "instance_id", id, "error", err)
		} else {
			releaseNetworkSpanEnd(nil)
		}
	}

	// 7. Release the vGPU assignment if present.
	if err := m.releaseStoredVGPU(ctx, stored); err != nil {
		log.WarnContext(ctx, "failed to destroy vGPU on stop; retaining assignment metadata", "instance_id", id, "error", err)
	}

	// 8. Always remove stale runtime sockets after process exit.
	// If graceful guest shutdown exits before shutdownHypervisor() is called, these
	// files may still exist and cause state derivation as Unknown or bind conflicts.
	_ = os.Remove(inst.SocketPath)
	_ = os.Remove(inst.VsockSocket)
	if matches, err := filepath.Glob(inst.VsockSocket + "_*"); err == nil {
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}
	m.closeFirecrackerUFFDSession(ctx, stored)

	// 9. Ensure terminal stop semantics: no snapshot should remain in Stopped state.
	// This prevents stale snapshot directories from deriving state as Standby and
	// blocking future StartInstance calls with invalid_state.
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	if err := os.RemoveAll(snapshotDir); err != nil {
		log.WarnContext(ctx, "failed to remove stale snapshot directory on stop", "instance_id", id, "snapshot_dir", snapshotDir, "error", err)
	}
	if m.supportsSnapshotBaseReuse(stored.HypervisorType) {
		retainedBaseDir := m.paths.InstanceSnapshotBase(id)
		if err := os.RemoveAll(retainedBaseDir); err != nil {
			log.WarnContext(ctx, "failed to remove retained snapshot base on stop", "instance_id", id, "snapshot_dir", retainedBaseDir, "error", err)
		}
	}

	// 10. Update metadata (clear PID, set StoppedAt)
	now := time.Now().UTC()
	stored.StoppedAt = &now
	stored.HypervisorPID = nil
	stored.HypervisorStartTime = 0
	// Boot markers are per-boot-run and must not carry across stop/restore/start.
	stored.ProgramStartedAt = nil
	stored.GuestAgentReadyAt = nil
	stored.FirecrackerSnapshotCacheKey = ""
	clearFirecrackerUFFDRestoreState(stored)
	stored.Phases.Record(phasetracking.PhaseStopped, now)

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 11. Persist exit info from serial console (under lock, safe from races)
	m.persistExitInfo(ctx, id)

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.stopDuration, start, "success", stored.HypervisorType)
		m.recordStateTransition(ctx, string(inst.State), string(StateStopped), stored.HypervisorType)
	}

	// Return instance with derived state (should be Stopped now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance stopped successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}
