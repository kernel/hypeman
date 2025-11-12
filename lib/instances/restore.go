package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/onkernel/hypeman/lib/vmm"
)

// RestoreInstance restores an instance from standby
// Multi-hop orchestration: Standby → Paused → Running
func (m *manager) restoreInstance(
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

	// 2. Validate state
	if inst.State != StateStandby {
		return nil, fmt.Errorf("%w: cannot restore from state %s", ErrInvalidState, inst.State)
	}

	if !inst.HasSnapshot {
		return nil, fmt.Errorf("no snapshot available for instance %s", id)
	}

	// 3. Get snapshot directory
	snapshotDir := filepath.Join(stored.DataDir, "snapshots", "snapshot-latest")

	// 4. Transition: Standby → Paused (start VMM + restore)
	if err := m.restoreFromSnapshot(ctx, stored, snapshotDir); err != nil {
		return nil, err
	}

	// 5. Create client for resumed VM
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("create vmm client: %w", err)
	}

	// 6. Transition: Paused → Running (resume)
	resumeResp, err := client.ResumeVMWithResponse(ctx)
	if err != nil || resumeResp.StatusCode() != 204 {
		return nil, fmt.Errorf("resume vm failed: %w", err)
	}

	// 7. Delete snapshot after successful restore
	os.RemoveAll(snapshotDir) // Best effort, ignore errors

	// 8. Update timestamp
	now := time.Now()
	stored.StartedAt = &now

	meta = &metadata{StoredMetadata: *stored}
	if err := m.saveMetadata(meta); err != nil {
		// VM is running but metadata failed
	}

	// Return instance with derived state (should be Running now)
	finalInst := m.toInstance(ctx, meta)
	return &finalInst, nil
}

// restoreFromSnapshot starts VMM and restores from snapshot
func (m *manager) restoreFromSnapshot(
	ctx context.Context,
	stored *StoredMetadata,
	snapshotDir string,
) error {
	// Start VMM process and capture PID
	pid, err := vmm.StartProcess(ctx, m.dataDir, stored.CHVersion, stored.SocketPath)
	if err != nil {
		return fmt.Errorf("start vmm: %w", err)
	}
	
	// Store the PID for later cleanup
	stored.CHPID = &pid

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

	resp, err := client.PutVmRestoreWithResponse(ctx, restoreConfig)
	if err != nil {
		client.ShutdownVMMWithResponse(ctx) // Cleanup
		return fmt.Errorf("restore api call: %w", err)
	}
	if resp.StatusCode() != 204 {
		client.ShutdownVMMWithResponse(ctx) // Cleanup
		return fmt.Errorf("restore failed with status %d", resp.StatusCode())
	}

	return nil
}


