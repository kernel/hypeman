package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/trace"
)

// DefaultStopTimeout is the default grace period for graceful shutdown (seconds).
// Similar to Docker's default of 10s.
const DefaultStopTimeout = 10

// stopInstance gracefully stops a running instance.
// Flow: send Shutdown RPC -> wait for VM to power off -> fall back to hard kill.
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
	// then wait for VM to power off cleanly. Fall back to hard kill on timeout.
	stopTimeout := stored.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = DefaultStopTimeout
	}

	gracefulShutdown := false
	if !stored.SkipGuestAgent {
		log.DebugContext(ctx, "sending graceful shutdown signal to guest", "instance_id", id, "timeout_seconds", stopTimeout)
		dialer, dialerErr := hypervisor.NewVsockDialer(stored.HypervisorType, stored.VsockSocket, stored.VsockCID)
		if dialerErr != nil {
			log.WarnContext(ctx, "could not create vsock dialer for graceful shutdown", "instance_id", id, "error", dialerErr)
		} else {
			// Send shutdown signal (best-effort, fire and forget)
			shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := guest.ShutdownInstance(shutdownCtx, dialer, 0); err != nil {
				log.WarnContext(ctx, "shutdown RPC failed (will hard kill)", "instance_id", id, "error", err)
			}
			cancel()

			// Wait for the hypervisor process to exit (init calls reboot(POWER_OFF))
			if inst.HypervisorPID != nil {
				if WaitForProcessExit(*inst.HypervisorPID, time.Duration(stopTimeout)*time.Second) {
					log.DebugContext(ctx, "VM shut down gracefully", "instance_id", id)
					gracefulShutdown = true
				} else {
					log.WarnContext(ctx, "graceful shutdown timed out, falling back to hard kill", "instance_id", id)
				}
			}
		}
	}

	// 5. Hard kill if graceful shutdown didn't work
	if !gracefulShutdown {
		log.DebugContext(ctx, "shutting down hypervisor (hard kill)", "instance_id", id)
		if err := m.shutdownHypervisor(ctx, &inst); err != nil {
			// Log but continue - try to clean up anyway
			log.WarnContext(ctx, "failed to shutdown hypervisor", "instance_id", id, "error", err)
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

	// 8. Update metadata (clear PID, mdev UUID, set StoppedAt)
	now := time.Now()
	stored.StoppedAt = &now
	stored.HypervisorPID = nil
	stored.GPUMdevUUID = "" // Clear mdev UUID since we destroyed it

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "instance_id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// 9. Persist exit info from serial console (under lock, safe from races)
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
