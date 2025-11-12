package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/onkernel/hypeman/lib/vmm"
)

// StandbyInstance puts an instance in standby state
// Multi-hop orchestration: Running → Paused → Standby
func (m *manager) standbyInstance(
	ctx context.Context,
	
	id string,
) (*Instance, error) {
	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata

	// 2. Validate state transition (must be Running to start standby flow)
	if inst.State != StateRunning {
		return nil, fmt.Errorf("%w: cannot standby from state %s", ErrInvalidState, inst.State)
	}

	// 3. Create VMM client
	client, err := vmm.NewVMM(inst.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("create vmm client: %w", err)
	}

	// 4. Reduce memory to base size (virtio-mem hotplug)
	if err := reduceMemory(ctx, client, inst.Size); err != nil {
		// Log warning but continue - snapshot will just be larger
	}

	// 5. Transition: Running → Paused
	pauseResp, err := client.PauseVMWithResponse(ctx)
	if err != nil || pauseResp.StatusCode() != 204 {
		return nil, fmt.Errorf("pause vm failed: %w", err)
	}

	// 6. Create snapshot
	snapshotDir := filepath.Join(stored.DataDir, "snapshots", "snapshot-latest")
	if err := createSnapshot(ctx, client, snapshotDir); err != nil {
		// Snapshot failed - try to resume VM
		client.ResumeVMWithResponse(ctx)
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	// 7. Stop VMM gracefully (snapshot is complete)
	if err := m.shutdownVMM(ctx, &inst); err != nil {
		// Log but continue - snapshot was created successfully
	}

	// 8. Update timestamp and clear PID (VMM no longer running)
	now := time.Now()
	stored.StoppedAt = &now
	stored.CHPID = nil

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Return instance with derived state (should be Standby now)
	finalInst := m.toInstance(ctx, meta)
	return &finalInst, nil
}

// reduceMemory attempts to reduce VM memory to minimize snapshot size
func reduceMemory(ctx context.Context, client *vmm.VMM, targetBytes int64) error {
	resizeConfig := vmm.VmResize{DesiredRam: &targetBytes}
	if resp, err := client.PutVmResizeWithResponse(ctx, resizeConfig); err != nil || resp.StatusCode() != 204 {
		return fmt.Errorf("memory resize failed")
	}

	// Wait for memory to be reduced
	// TODO: Poll actual memory usage instead of fixed sleep
	time.Sleep(2 * time.Second)

	return nil
}

// createSnapshot creates a Cloud Hypervisor snapshot
func createSnapshot(ctx context.Context, client *vmm.VMM, snapshotDir string) error {
	// Remove old snapshot
	os.RemoveAll(snapshotDir)

	// Create snapshot directory
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	// Create snapshot via VMM API
	snapshotURL := "file://" + snapshotDir
	snapshotConfig := vmm.VmSnapshotConfig{DestinationUrl: &snapshotURL}

	resp, err := client.PutVmSnapshotWithResponse(ctx, snapshotConfig)
	if err != nil {
		return fmt.Errorf("snapshot api call: %w", err)
	}
	if resp.StatusCode() != 204 {
		return fmt.Errorf("snapshot failed with status %d", resp.StatusCode())
	}

	return nil
}

// shutdownVMM gracefully shuts down the VMM process via API
func (m *manager) shutdownVMM(ctx context.Context, inst *Instance) error {
	// Try to connect to VMM
	client, err := vmm.NewVMM(inst.SocketPath)
	if err != nil {
		// Can't connect - VMM might already be stopped
		return nil
	}

	// Try graceful shutdown
	client.ShutdownVMMWithResponse(ctx)
	
	// Wait for process to exit
	if inst.CHPID != nil {
		waitForProcessExit(*inst.CHPID, 2*time.Second)
	}
	
	return nil
}

