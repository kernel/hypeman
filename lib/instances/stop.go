package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/trace"
)

// stopInstance gracefully stops a running instance
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

	// 4. Shutdown hypervisor process
	// TODO: Add graceful shutdown via vsock signal to allow app to clean up
	log.DebugContext(ctx, "shutting down hypervisor", "instance_id", id)
	if err := m.shutdownHypervisor(ctx, &inst); err != nil {
		// Log but continue - try to clean up anyway
		log.WarnContext(ctx, "failed to shutdown hypervisor gracefully", "instance_id", id, "error", err)
	}

	// 5. Release network allocation (delete TAP device)
	if inst.NetworkEnabled && networkAlloc != nil {
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue
			log.WarnContext(ctx, "failed to release network, continuing", "instance_id", id, "error", err)
		}
	}

	// 6. Update metadata (clear PID, set StoppedAt)
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
		m.recordDuration(ctx, m.metrics.stopDuration, start, "success", stored.HypervisorType)
		m.recordStateTransition(ctx, string(StateRunning), string(StateStopped), stored.HypervisorType)
	}

	// Return instance with derived state (should be Stopped now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance stopped successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}
