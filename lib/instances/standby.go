package instances

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/vmm"
)

// StandbyInstance puts an instance in standby state
// Multi-hop orchestration: Running → Paused → Standby
func (m *manager) standbyInstance(
	ctx context.Context,
	
	id string,
) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "putting instance in standby", "id", id)
	
	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	log.DebugContext(ctx, "loaded instance", "id", id, "state", inst.State)

	// 2. Validate state transition (must be Running to start standby flow)
	if inst.State != StateRunning {
		log.ErrorContext(ctx, "invalid state for standby", "id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot standby from state %s", ErrInvalidState, inst.State)
	}

	// 3. Get network allocation BEFORE killing VMM (while we can still query it)
	// This is needed to delete the TAP device after VMM shuts down
	var networkAlloc *network.Allocation
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "getting network allocation", "id", id)
		networkAlloc, err = m.networkManager.GetAllocation(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "failed to get network allocation, will still attempt cleanup", "id", id, "error", err)
		}
	}

	// 4. Create VMM client
	client, err := vmm.NewVMM(inst.SocketPath)
	if err != nil {
		log.ErrorContext(ctx, "failed to create VMM client", "id", id, "error", err)
		return nil, fmt.Errorf("create vmm client: %w", err)
	}

	// 5. Reduce memory to base size (virtio-mem hotplug)
	log.DebugContext(ctx, "reducing VM memory before snapshot", "id", id, "base_size", inst.Size)
	if err := reduceMemory(ctx, client, inst.Size); err != nil {
		// Log warning but continue - snapshot will just be larger
		log.WarnContext(ctx, "failed to reduce memory, snapshot will be larger", "id", id, "error", err)
	}

	// 6. Transition: Running → Paused
	log.DebugContext(ctx, "pausing VM", "id", id)
	pauseResp, err := client.PauseVMWithResponse(ctx)
	if err != nil || pauseResp.StatusCode() != 204 {
		log.ErrorContext(ctx, "failed to pause VM", "id", id, "error", err)
		return nil, fmt.Errorf("pause vm failed: %w", err)
	}

	// 7. Create snapshot
	snapshotDir := m.paths.InstanceSnapshotLatest(id)
	log.DebugContext(ctx, "creating snapshot", "id", id, "snapshot_dir", snapshotDir)
	if err := createSnapshot(ctx, client, snapshotDir); err != nil {
		// Snapshot failed - try to resume VM
		log.ErrorContext(ctx, "snapshot failed, attempting to resume VM", "id", id, "error", err)
		client.ResumeVMWithResponse(ctx)
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	// 8. Stop VMM gracefully (snapshot is complete)
	log.DebugContext(ctx, "shutting down VMM", "id", id)
	if err := m.shutdownVMM(ctx, &inst); err != nil {
		// Log but continue - snapshot was created successfully
		log.WarnContext(ctx, "failed to shutdown VMM gracefully, snapshot still valid", "id", id, "error", err)
	}

	// 9. Release network allocation (delete TAP device)
	// TAP devices with explicit Owner/Group fields do NOT auto-delete when VMM exits
	// They must be explicitly deleted
	if inst.NetworkEnabled {
		log.DebugContext(ctx, "releasing network", "id", id, "network", "default")
		if err := m.networkManager.ReleaseAllocation(ctx, networkAlloc); err != nil {
			// Log error but continue - snapshot was created successfully
			log.WarnContext(ctx, "failed to release network, continuing with standby", "id", id, "error", err)
		}
	}

	// 10. Update timestamp and clear PID (VMM no longer running)
	now := time.Now()
	stored.StoppedAt = &now
	stored.CHPID = nil

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		log.ErrorContext(ctx, "failed to save metadata", "id", id, "error", err)
		return nil, fmt.Errorf("save metadata: %w", err)
	}

	// Return instance with derived state (should be Standby now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance put in standby successfully", "id", id, "state", finalInst.State)
	return &finalInst, nil
}

// reduceMemory attempts to reduce VM memory to minimize snapshot size
func reduceMemory(ctx context.Context, client *vmm.VMM, targetBytes int64) error {
	resizeConfig := vmm.VmResize{DesiredRam: &targetBytes}
	if resp, err := client.PutVmResizeWithResponse(ctx, resizeConfig); err != nil || resp.StatusCode() != 204 {
		return fmt.Errorf("memory resize failed")
	}

	// Poll actual memory usage until it reaches target size
	return pollVMMemory(ctx, client, targetBytes, 5*time.Second)
}

// pollVMMemory polls VM memory usage until it reduces and stabilizes
func pollVMMemory(ctx context.Context, client *vmm.VMM, targetBytes int64, timeout time.Duration) error {
	log := logger.FromContext(ctx)
	deadline := time.Now().Add(timeout)
	
	// Use 20ms for fast response with minimal overhead
	const pollInterval = 20 * time.Millisecond
	const stabilityThreshold = 3 // Memory unchanged for 3 checks = stable
	
	var previousSize *int64
	unchangedCount := 0
	
	for time.Now().Before(deadline) {
		infoResp, err := client.GetVmInfoWithResponse(ctx)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		
		if infoResp.StatusCode() != 200 || infoResp.JSON200 == nil {
			time.Sleep(pollInterval)
			continue
		}
		
		actualSize := infoResp.JSON200.MemoryActualSize
		if actualSize == nil {
			time.Sleep(pollInterval)
			continue
		}
		
		currentSize := *actualSize
		
		// Best case: reached target or below
		if currentSize <= targetBytes {
			log.DebugContext(ctx, "memory reduced to target",
				"target_mb", targetBytes/(1024*1024),
				"actual_mb", currentSize/(1024*1024))
			return nil
		}
		
		// Check if memory has stopped shrinking (stabilized above target)
		if previousSize != nil {
			if currentSize == *previousSize {
				unchangedCount++
				if unchangedCount >= stabilityThreshold {
					// Memory has stabilized but above target
					// Guest OS couldn't free more memory - accept this as "done"
					log.WarnContext(ctx, "memory reduction stabilized above target",
						"target_mb", targetBytes/(1024*1024),
						"actual_mb", currentSize/(1024*1024),
						"diff_mb", (currentSize-targetBytes)/(1024*1024))
					return nil // Not an error - snapshot will just be larger
				}
			} else if currentSize < *previousSize {
				// Still shrinking - reset counter
				unchangedCount = 0
			}
		}
		
		previousSize = &currentSize
		time.Sleep(pollInterval)
	}
	
	// Timeout - memory never stabilized
	return fmt.Errorf("memory reduction did not complete within %v", timeout)
}

// createSnapshot creates a Cloud Hypervisor snapshot
func createSnapshot(ctx context.Context, client *vmm.VMM, snapshotDir string) error {
	log := logger.FromContext(ctx)
	
	// Remove old snapshot
	os.RemoveAll(snapshotDir)

	// Create snapshot directory
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	// Create snapshot via VMM API
	snapshotURL := "file://" + snapshotDir
	snapshotConfig := vmm.VmSnapshotConfig{DestinationUrl: &snapshotURL}

	log.DebugContext(ctx, "invoking VMM snapshot API", "snapshot_url", snapshotURL)
	resp, err := client.PutVmSnapshotWithResponse(ctx, snapshotConfig)
	if err != nil {
		return fmt.Errorf("snapshot api call: %w", err)
	}
	if resp.StatusCode() != 204 {
		log.ErrorContext(ctx, "snapshot API returned error", "status", resp.StatusCode())
		return fmt.Errorf("snapshot failed with status %d", resp.StatusCode())
	}

	log.DebugContext(ctx, "snapshot created successfully", "snapshot_dir", snapshotDir)
	return nil
}

// shutdownVMM gracefully shuts down the VMM process via API
func (m *manager) shutdownVMM(ctx context.Context, inst *Instance) error {
	log := logger.FromContext(ctx)
	
	// Try to connect to VMM
	client, err := vmm.NewVMM(inst.SocketPath)
	if err != nil {
		// Can't connect - VMM might already be stopped
		log.DebugContext(ctx, "could not connect to VMM, may already be stopped", "id", inst.Id)
		return nil
	}

	// Try graceful shutdown
	log.DebugContext(ctx, "sending shutdown command to VMM", "id", inst.Id)
	client.ShutdownVMMWithResponse(ctx)
	
	// Wait for process to exit
	if inst.CHPID != nil {
		if !WaitForProcessExit(*inst.CHPID, 2*time.Second) {
			log.WarnContext(ctx, "VMM did not exit gracefully in time", "id", inst.Id, "pid", *inst.CHPID)
		} else {
			log.DebugContext(ctx, "VMM shutdown gracefully", "id", inst.Id, "pid", *inst.CHPID)
		}
	}
	
	return nil
}

