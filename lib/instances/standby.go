package instances

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
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

	// 6. Reduce memory to base size (virtio-mem hotplug) if supported
	if hv.Capabilities().SupportsHotplugMemory {
		log.DebugContext(ctx, "reducing VM memory before snapshot", "instance_id", id, "base_size", inst.Size)
		if err := hv.ResizeMemory(ctx, inst.Size); err != nil {
			// Log warning but continue - snapshot will just be larger
			log.WarnContext(ctx, "failed to reduce memory, snapshot will be larger", "instance_id", id, "error", err)
		}
	}

	// 7. Transition: Running → Paused
	log.DebugContext(ctx, "pausing VM", "instance_id", id)
	if err := hv.Pause(ctx); err != nil {
		log.ErrorContext(ctx, "failed to pause VM", "instance_id", id, "error", err)
		return nil, fmt.Errorf("pause vm failed: %w", err)
	}

	// 8. Create snapshot
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	log.DebugContext(ctx, "creating snapshot", "instance_id", id, "snapshot_dir", snapshotDir)
	if err := createSnapshot(ctx, hv, snapshotDir); err != nil {
		// Snapshot failed - try to resume VM
		log.ErrorContext(ctx, "snapshot failed, attempting to resume VM", "instance_id", id, "error", err)
		hv.Resume(ctx)
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	// 9. Stop VMM gracefully (snapshot is complete)
	log.DebugContext(ctx, "shutting down hypervisor", "instance_id", id)
	if err := m.shutdownHypervisor(ctx, &inst); err != nil {
		// Log but continue - snapshot was created successfully
		log.WarnContext(ctx, "failed to shutdown hypervisor gracefully, snapshot still valid", "instance_id", id, "error", err)
	}

	// 10. Release network allocation (delete TAP device)
	// TAP devices with explicit Owner/Group fields do NOT auto-delete when VMM exits
	// They must be explicitly deleted
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "releasing network", "instance_id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue - snapshot was created successfully
			log.WarnContext(ctx, "failed to release network, continuing with standby", "instance_id", id, "error", err)
		}
	}

	// 11. Update timestamp and clear PID (hypervisor no longer running)
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
		m.recordDuration(ctx, m.metrics.standbyDuration, start, "success")
		m.recordStateTransition(ctx, string(StateRunning), string(StateStandby))
	}

	// Return instance with derived state (should be Standby now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance put in standby successfully", "instance_id", id, "state", finalInst.State)
	return &finalInst, nil
}

// createSnapshot creates a snapshot using the hypervisor interface
func createSnapshot(ctx context.Context, hv hypervisor.Hypervisor, snapshotDir string) error {
	log := logger.FromContext(ctx)

	// Remove old snapshot
	os.RemoveAll(snapshotDir)

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

// shutdownHypervisor gracefully shuts down the hypervisor process via API
func (m *manager) shutdownHypervisor(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)

	// Try to connect to hypervisor
	hv, err := m.getHypervisor(inst.SocketPath, inst.HypervisorType)
	if err != nil {
		// Can't connect - hypervisor might already be stopped
		log.DebugContext(ctx, "could not connect to hypervisor, may already be stopped", "instance_id", inst.Id)
		return nil
	}

	// Try graceful shutdown
	log.DebugContext(ctx, "sending shutdown command to hypervisor", "instance_id", inst.Id)
	hv.Shutdown(ctx)

	// Wait for process to exit
	if inst.HypervisorPID != nil {
		if !WaitForProcessExit(*inst.HypervisorPID, 2*time.Second) {
			log.WarnContext(ctx, "hypervisor did not exit gracefully in time", "instance_id", inst.Id, "pid", *inst.HypervisorPID)
		} else {
			log.DebugContext(ctx, "hypervisor shutdown gracefully", "instance_id", inst.Id, "pid", *inst.HypervisorPID)
		}
	}

	return nil
}
