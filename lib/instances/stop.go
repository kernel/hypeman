package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/trace"
)

// DefaultStopTimeout is the default grace period for graceful shutdown (seconds).
const DefaultStopTimeout = 5
const shutdownRPCDeadline = 1500 * time.Millisecond
const shutdownFailureFallbackWait = 500 * time.Millisecond

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

	if inst.HypervisorPID == nil {
		return nil
	}

	pid := *inst.HypervisorPID
	if err := syscall.Kill(pid, 0); err != nil {
		// Process is already gone (likely ESRCH).
		return nil
	}

	log.WarnContext(ctx, "hypervisor still running after shutdown fallback, sending SIGKILL", "instance_id", inst.Id, "pid", pid)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("sigkill hypervisor pid %d: %w", pid, err)
	}

	// Wait for process to die and reap it to avoid zombie false positives.
	reaped := false
	for i := 0; i < 50; i++ { // 50 * 100ms = 5s
		var wstatus syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &wstatus, syscall.WNOHANG, nil)
		if err != nil || wpid == pid {
			// Process reaped, or not our child (ECHILD) and no longer trackable here.
			reaped = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !reaped {
		// Timed out waiting for reap; if process still exists, treat as failure.
		if err := syscall.Kill(pid, 0); err == nil {
			return fmt.Errorf("hypervisor pid %d still alive after SIGKILL", pid)
		}
		log.WarnContext(ctx, "timeout waiting to reap hypervisor process after SIGKILL", "instance_id", inst.Id, "pid", pid)
	}

	log.DebugContext(ctx, "hypervisor process force-killed", "instance_id", inst.Id, "pid", pid)
	return nil
}

// stopInstance gracefully stops a running instance.
// Flow: send Shutdown RPC -> wait for VM to power off ->
// fall back to hypervisor shutdown -> final SIGKILL if still alive.
// Multi-hop orchestration: Running → Shutdown → Stopped
func (m *manager) stopInstance(
	ctx context.Context,
	id string,
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "stopping instance", "instance_id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "StopInstance")
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

	// 2. Validate state transition (must be Running to stop)
	if inst.State != StateRunning {
		log.ErrorContext(ctx, "invalid state for stop", "instance_id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot stop from state %s, must be Running", ErrInvalidState, inst.State)
	}

	// 3. Get network allocation BEFORE killing VMM (while we can still query it)
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "instance_id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "instance_id", id, "error", err)
		}
	}

	// 4. Graceful shutdown: send signal to guest init via Shutdown RPC,
	// then wait for VM to power off cleanly. Fall back to hypervisor shutdown on timeout.
	stopTimeout := resolveStopTimeout(stored)
	gracefulShutdown := m.tryGracefulGuestShutdown(ctx, &inst, stopTimeout)

	// 5. Fallback hypervisor shutdown if guest graceful shutdown didn't work
	if !gracefulShutdown {
		log.DebugContext(ctx, "shutting down hypervisor (fallback)", "instance_id", id)
		if err := m.shutdownHypervisor(ctx, &inst); err != nil {
			// Continue to final SIGKILL fallback if graceful shutdown API fails.
			log.WarnContext(ctx, "failed to shutdown hypervisor", "instance_id", id, "error", err)
		}

		// Final fallback: force-kill the process if it's still alive.
		if err := m.forceKillHypervisorProcess(ctx, &inst); err != nil {
			log.ErrorContext(ctx, "failed to force-kill hypervisor process", "instance_id", id, "error", err)
			return nil, err
		}
	}

	// 6. Release network allocation (delete TAP device)
	if inst.NetworkEnabled && networkAlloc != nil {
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue
			log.WarnContext(ctx, "failed to release network, continuing", "instance_id", id, "error", err)
		}
	}

	// 7. Destroy vGPU mdev device if present (frees vGPU slot for other VMs)
	if inst.GPUMdevUUID != "" {
		log.InfoContext(ctx, "destroying vGPU mdev on stop", "instance_id", id, "uuid", inst.GPUMdevUUID)
		if err := devices.DestroyMdev(ctx, inst.GPUMdevUUID); err != nil {
			// Log error but continue - mdev cleanup is best-effort
			log.WarnContext(ctx, "failed to destroy mdev on stop", "instance_id", id, "uuid", inst.GPUMdevUUID, "error", err)
		}
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

	// 9. Ensure terminal stop semantics: no snapshot should remain in Stopped state.
	// This prevents stale snapshot directories from deriving state as Standby and
	// blocking future StartInstance calls with invalid_state.
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	if err := os.RemoveAll(snapshotDir); err != nil {
		log.WarnContext(ctx, "failed to remove stale snapshot directory on stop", "instance_id", id, "snapshot_dir", snapshotDir, "error", err)
	}

	// 10. Update metadata (clear PID, mdev UUID, set StoppedAt)
	now := time.Now()
	stored.StoppedAt = &now
	stored.HypervisorPID = nil
	stored.GPUMdevUUID = "" // Clear mdev UUID since we destroyed it

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
		m.recordStateTransition(ctx, string(StateRunning), string(StateStopped), stored.HypervisorType)
	}

	// Return instance with derived state (should be Stopped now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance stopped successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}
