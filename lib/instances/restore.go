package instances

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/vmm"
)

// RestoreInstance restores an instance from standby
// Multi-hop orchestration: Standby → Paused → Running
func (m *manager) restoreInstance(
	ctx context.Context,
	
	id string,
) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.InfoContext(ctx, "restoring instance from standby", "id", id)
	
	// 1. Load instance
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	stored := &meta.StoredMetadata
	log.DebugContext(ctx, "loaded instance", "id", id, "state", inst.State, "has_snapshot", inst.HasSnapshot)

	// 2. Validate state
	if inst.State != StateStandby {
		log.ErrorContext(ctx, "invalid state for restore", "id", id, "state", inst.State)
		return nil, fmt.Errorf("%w: cannot restore from state %s", ErrInvalidState, inst.State)
	}

	if !inst.HasSnapshot {
		log.ErrorContext(ctx, "no snapshot available", "id", id)
		return nil, fmt.Errorf("no snapshot available for instance %s", id)
	}

	// 3. Get snapshot directory
	snapshotDir := m.paths.InstanceSnapshotLatest(id)

	// 4. Recreate TAP device if network configured
	if stored.Network != "" {
		log.DebugContext(ctx, "recreating network for restore", "id", id, "network", stored.Network)
		if err := m.networkManager.RecreateNetwork(ctx, id); err != nil {
			log.ErrorContext(ctx, "failed to recreate network", "id", id, "error", err)
			return nil, fmt.Errorf("recreate network: %w", err)
		}
	}

	// 5. Transition: Standby → Paused (start VMM + restore)
	log.DebugContext(ctx, "restoring from snapshot", "id", id, "snapshot_dir", snapshotDir)
	if err := m.restoreFromSnapshot(ctx, stored, snapshotDir); err != nil {
		log.ErrorContext(ctx, "failed to restore from snapshot", "id", id, "error", err)
		// Cleanup network on failure
		if stored.Network != "" {
			m.networkManager.ReleaseNetwork(ctx, id)
		}
		return nil, err
	}

	// 6. Create client for resumed VM
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		log.ErrorContext(ctx, "failed to create VMM client", "id", id, "error", err)
		// Cleanup network on failure
		// TODO @sjmiller609 review: seeing lots of places where we clean up the network, but what does this mean, why not automatically do it if instance is disappeared?
		if stored.Network != "" {
			m.networkManager.ReleaseNetwork(ctx, id)
		}
		return nil, fmt.Errorf("create vmm client: %w", err)
	}

	// 7. Transition: Paused → Running (resume)
	log.DebugContext(ctx, "resuming VM", "id", id)
	resumeResp, err := client.ResumeVMWithResponse(ctx)
	if err != nil || resumeResp.StatusCode() != 204 {
		log.ErrorContext(ctx, "failed to resume VM", "id", id, "error", err)
		// Cleanup network on failure
		if stored.Network != "" {
			m.networkManager.ReleaseNetwork(ctx, id)
		}
		return nil, fmt.Errorf("resume vm failed: %w", err)
	}

	// 8. Delete snapshot after successful restore
	log.DebugContext(ctx, "deleting snapshot after successful restore", "id", id)
	os.RemoveAll(snapshotDir) // Best effort, ignore errors

	// 9. Update timestamp
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed
		log.WarnContext(ctx, "failed to update metadata after restore", "id", id, "error", err)
	}

	// Return instance with derived state (should be Running now)
	finalInst := m.toInstance(ctx, meta)
	log.InfoContext(ctx, "instance restored successfully", "id", id, "state", finalInst.State)
	return &finalInst, nil
}

// restoreFromSnapshot starts VMM and restores from snapshot
func (m *manager) restoreFromSnapshot(
	ctx context.Context,
	stored *StoredMetadata,
	snapshotDir string,
) error {
	log := logger.FromContext(ctx)
	
	// Start VMM process and capture PID
	log.DebugContext(ctx, "starting VMM process for restore", "id", stored.Id, "version", stored.CHVersion)
	pid, err := vmm.StartProcess(ctx, m.paths, stored.CHVersion, stored.SocketPath)
	if err != nil {
		return fmt.Errorf("start vmm: %w", err)
	}
	
	// Store the PID for later cleanup
	stored.CHPID = &pid
	log.DebugContext(ctx, "VMM process started", "id", stored.Id, "pid", pid)

	// Create client
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		return fmt.Errorf("create vmm client: %w", err)
	}

	// Restore from snapshot
	sourceURL := "file://" + snapshotDir
	restoreConfig := vmm.RestoreConfig{
		SourceUrl: sourceURL,
		Prefault:  ptr(false), // Don't prefault pages for faster restore
	}

	log.DebugContext(ctx, "invoking VMM restore API", "id", stored.Id, "source_url", sourceURL)
	resp, err := client.PutVmRestoreWithResponse(ctx, restoreConfig)
	if err != nil {
		log.ErrorContext(ctx, "restore API call failed", "id", stored.Id, "error", err)
		client.ShutdownVMMWithResponse(ctx) // Cleanup
		return fmt.Errorf("restore api call: %w", err)
	}
	if resp.StatusCode() != 204 {
		log.ErrorContext(ctx, "restore API returned error", "id", stored.Id, "status", resp.StatusCode())
		client.ShutdownVMMWithResponse(ctx) // Cleanup
		return fmt.Errorf("restore failed with status %d", resp.StatusCode())
	}

	log.DebugContext(ctx, "VM restored from snapshot successfully", "id", stored.Id)
	return nil
}


