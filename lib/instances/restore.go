package instances

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/trace"
)

// RestoreInstance restores an instance from standby
// Multi-hop orchestration: Standby → Paused → Running
func (m *manager) restoreInstance(
	ctx context.Context,

	id string,
) (*Instance, error) {
	start := time.Now()
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "restoring instance from standby", "instance_id", id)

	// Start tracing span if tracer is available
	if m.metrics != nil && m.metrics.tracer != nil {
		var span trace.Span
		ctx, span = m.metrics.tracer.Start(ctx, "RestoreInstance")
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
	log.DebugContext(ctx, "loaded instance", "instance_id", id, "state", inst.State, "has_snapshot", inst.HasSnapshot)

	// 2. Validate state
	if inst.State != StateStandby {
		log.ErrorContext(ctx, "invalid state for restore", "instance_id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot restore from state %s", ErrInvalidState, inst.State)
	}

	if !inst.HasSnapshot {
		log.ErrorContext(ctx, "no snapshot available", "instance_id", id)
		return nil, fmt.Errorf("no snapshot available for instance %s", id)
	}

	// 3. Get snapshot directory
	snapshotDir := m.paths.InstanceSnapshotLatest(id)

	// 4. Recreate TAP device if network enabled
	if stored.NetworkEnabled {
		var networkSpan trace.Span
		if m.metrics != nil && m.metrics.tracer != nil {
			ctx, networkSpan = m.metrics.tracer.Start(ctx, "RestoreNetwork")
		}
		log.InfoContext(ctx, "recreating network for restore", "instance_id", id, "network", "default", "network_bandwidth", stored.NetworkBandwidth)
		if err := m.networkManager.RecreateAllocation(ctx, id, stored.NetworkBandwidth); err != nil {
			if networkSpan != nil {
				networkSpan.End()
			}
			log.ErrorContext(ctx, "failed to recreate network", "instance_id", id, "error", err)
			return nil, fmt.Errorf("recreate network: %w", err)
		}
		if networkSpan != nil {
			networkSpan.End()
		}
	}

	// 5. Transition: Standby → Paused (start hypervisor + restore)
	var restoreSpan trace.Span
	if m.metrics != nil && m.metrics.tracer != nil {
		ctx, restoreSpan = m.metrics.tracer.Start(ctx, "RestoreFromSnapshot")
	}
	log.InfoContext(ctx, "restoring from snapshot", "instance_id", id, "snapshot_dir", snapshotDir, "hypervisor", stored.HypervisorType)
	pid, hv, err := m.restoreFromSnapshot(ctx, stored, snapshotDir)
	if restoreSpan != nil {
		restoreSpan.End()
	}
	if err != nil {
		log.ErrorContext(ctx, "failed to restore from snapshot", "instance_id", id, "error", err)
		// Cleanup network on failure
		if stored.NetworkEnabled {
			netAlloc, _ := m.networkManager.GetAllocation(ctx, id)
			m.networkManager.ReleaseAllocation(ctx, netAlloc)
		}
		return nil, err
	}

	// Store the PID for later cleanup
	stored.HypervisorPID = &pid

	// 6. Transition: Paused → Running (resume)
	var resumeSpan trace.Span
	if m.metrics != nil && m.metrics.tracer != nil {
		ctx, resumeSpan = m.metrics.tracer.Start(ctx, "ResumeVM")
	}
	log.InfoContext(ctx, "resuming VM", "instance_id", id)
	if err := hv.Resume(ctx); err != nil {
		if resumeSpan != nil {
			resumeSpan.End()
		}
		log.ErrorContext(ctx, "failed to resume VM", "instance_id", id, "error", err)
		// Cleanup on failure
		hv.Shutdown(ctx)
		if stored.NetworkEnabled {
			netAlloc, _ := m.networkManager.GetAllocation(ctx, id)
			m.networkManager.ReleaseAllocation(ctx, netAlloc)
		}
		return nil, fmt.Errorf("resume vm failed: %w", err)
	}
	if resumeSpan != nil {
		resumeSpan.End()
	}

	// 8. Delete snapshot after successful restore
	log.InfoContext(ctx, "deleting snapshot after successful restore", "instance_id", id)
	os.RemoveAll(snapshotDir) // Best effort, ignore errors

	// 9. Update timestamp
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed
		log.WarnContext(ctx, "failed to update metadata after restore", "instance_id", id, "error", err)
	}

	// Record metrics
	if m.metrics != nil {
		m.recordDuration(ctx, m.metrics.restoreDuration, start, "success", stored.HypervisorType)
		m.recordStateTransition(ctx, string(StateStandby), string(StateRunning), stored.HypervisorType)
	}

	// Return instance with derived state (should be Running now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance restored successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}

// restoreFromSnapshot starts the hypervisor and restores from snapshot
func (m *manager) restoreFromSnapshot(
	ctx context.Context,
	stored *StoredMetadata,
	snapshotDir string,
) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// Get VM starter for this hypervisor type
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		return 0, nil, fmt.Errorf("get vm starter: %w", err)
	}

	// Restore VM from snapshot (handles process start + restore)
	log.DebugContext(ctx, "restoring VM from snapshot", "instance_id", stored.Id, "hypervisor", stored.HypervisorType, "version", stored.HypervisorVersion, "snapshot_dir", snapshotDir)
	pid, hv, err := starter.RestoreVM(ctx, m.paths, stored.HypervisorVersion, stored.SocketPath, snapshotDir)
	if err != nil {
		return 0, nil, fmt.Errorf("restore vm: %w", err)
	}

	log.DebugContext(ctx, "VM restored from snapshot successfully", "instance_id", stored.Id, "pid", pid)
	return pid, hv, nil
}
