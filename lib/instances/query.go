package instances

import (
	"context"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/vmm"
)

// deriveState determines instance state by checking socket and querying VMM
func (m *manager) deriveState(ctx context.Context, stored *StoredMetadata) State {
	// 1. Check if socket exists
	if _, err := os.Stat(stored.SocketPath); err != nil {
		// No socket - check for snapshot to distinguish Stopped vs Standby
		if m.hasSnapshot(stored.DataDir) {
			return StateStandby
		}
		return StateStopped
	}

	// 2. Socket exists - query VMM for actual state
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		// Stale socket - check for snapshot to distinguish Stopped vs Standby
		if m.hasSnapshot(stored.DataDir) {
			return StateStandby
		}
		return StateStopped
	}

	resp, err := client.GetVmInfoWithResponse(ctx)
	if err != nil {
		// VMM unreachable - stale socket, check for snapshot
		if m.hasSnapshot(stored.DataDir) {
			return StateStandby
		}
		return StateStopped
	}

	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		// VMM returned error - check for snapshot
		if m.hasSnapshot(stored.DataDir) {
			return StateStandby
		}
		return StateStopped
	}

	// 3. Map CH state to our state
	switch resp.JSON200.State {
	case vmm.Created:
		return StateCreated
	case vmm.Running:
		return StateRunning
	case vmm.Paused:
		return StatePaused
	case vmm.Shutdown:
		return StateShutdown
	default:
		return StateStopped
	}
}

// hasSnapshot checks if a snapshot exists for an instance
func (m *manager) hasSnapshot(dataDir string) bool {
	snapshotDir := filepath.Join(dataDir, "snapshots", "snapshot-latest")
	info, err := os.Stat(snapshotDir)
	if err != nil {
		return false
	}
	// Check directory exists and is not empty
	if !info.IsDir() {
		return false
	}
	// Read directory to check for any snapshot files
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// toInstance converts stored metadata to Instance with derived fields
func (m *manager) toInstance(ctx context.Context, meta *metadata) Instance {
	inst := Instance{
		StoredMetadata: meta.StoredMetadata,
		State:          m.deriveState(ctx, &meta.StoredMetadata),
		HasSnapshot:    m.hasSnapshot(meta.StoredMetadata.DataDir),
	}
	return inst
}

// listInstances returns all instances
func (m *manager) listInstances(ctx context.Context) ([]Instance, error) {
	log := logger.FromContext(ctx)
	log.DebugContext(ctx, "listing all instances")
	
	files, err := m.listMetadataFiles()
	if err != nil {
		log.ErrorContext(ctx, "failed to list metadata files", "error", err)
		return nil, err
	}

	result := make([]Instance, 0, len(files))
	for _, file := range files {
		// Extract instance ID from path
		// Path format: {dataDir}/guests/{id}/metadata.json
		id := filepath.Base(filepath.Dir(file))

		meta, err := m.loadMetadata(id)
		if err != nil {
			// Skip instances with invalid metadata
			log.WarnContext(ctx, "skipping instance with invalid metadata", "id", id, "error", err)
			continue
		}

		inst := m.toInstance(ctx, meta)
		result = append(result, inst)
	}

	log.DebugContext(ctx, "listed instances", "count", len(result))
	return result, nil
}

// getInstance returns a single instance by ID
func (m *manager) getInstance(ctx context.Context, id string) (*Instance, error) {
	log := logger.FromContext(ctx)
	log.DebugContext(ctx, "getting instance", "id", id)
	
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.ErrorContext(ctx, "failed to load instance metadata", "id", id, "error", err)
		return nil, err
	}

	inst := m.toInstance(ctx, meta)
	log.DebugContext(ctx, "retrieved instance", "id", id, "state", inst.State)
	return &inst, nil
}

