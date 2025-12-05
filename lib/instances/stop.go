package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/vmm"
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
	log.InfoContext(ctx, "stopping instance", "id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "StopInstance")
		defer span.End()
	}

	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	log.DebugContext(ctx, "loaded instance", "id", id, "state", inst.State)

	// 2. Validate state transition (must be Running to stop)
	if inst.State != StateRunning {
		log.ErrorContext(ctx, "invalid state for stop", "id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot stop from state %s, must be Running", ErrInvalidState, inst.State)
	}

	// 3. Get network allocation BEFORE killing VMM (while we can still query it)
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "id", id, "error", err)
		}
	}

	// 4. Create VMM client and send shutdown to guest
	client, err := vmm.NewVMM(inst.SocketPath)
	if err != nil {
		log.ErrorContext(ctx, "failed to create VMM client", "id", id, "error", err)
		return nil, fmt.Errorf("create vmm client: %w", err)
	}

	// 5. Transition: Running → Shutdown (graceful guest shutdown via ACPI)
	log.DebugContext(ctx, "sending shutdown to VM", "id", id)
	shutdownResp, err := client.ShutdownVMWithResponse(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to send shutdown to VM", "id", id, "error", err)
		return nil, fmt.Errorf("shutdown vm: %w", err)
	}
	if shutdownResp.StatusCode() != 204 {
		log.ErrorContext(ctx, "shutdown VM returned error", "id", id, "status", shutdownResp.StatusCode())
		return nil, fmt.Errorf("shutdown vm failed with status %d", shutdownResp.StatusCode())
	}

	// 6. Transition: Shutdown → Stopped (shutdown VMM process)
	log.DebugContext(ctx, "shutting down VMM", "id", id)
	if err := m.shutdownVMM(ctx, &inst); err != nil {
		// Log but continue - try to clean up anyway
		log.WarnContext(ctx, "failed to shutdown VMM gracefully", "id", id, "error", err)
	}

	// 7. Release network allocation (delete TAP device)
	if inst.NetworkEnabled && networkAlloc != nil {
		log.DebugContext(ctx, "releasing network", "id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue
			log.WarnContext(ctx, "failed to release network, continuing", "id", id, "error", err)
		}
	}

	// 8. Update metadata (clear PID, set StoppedAt)
	now := time.Now()
	stored.StoppedAt = &now
	stored.CHPID = nil

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.stopDuration, start, "success")
		m.recordStateTransition(ctx, string(StateRunning), string(StateStopped))
	}

	// Return instance with derived state (should be Stopped now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance stopped successfully", "id", id, "state", finalInst.State)
	return &finalInst, nil
}
