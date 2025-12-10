package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/vmm"
)

// stateResult holds the result of state derivation
type stateResult struct {
	State State
	Error *string // Non-nil if state couldn't be determined
}

// deriveState determines instance state by checking socket and querying VMM.
// Returns StateUnknown with an error message if the socket exists but VMM is unreachable.
func (m *manager) deriveState(ctx context.Context, stored *StoredMetadata) stateResult {
	log := logger.FromContext(ctx)

	// 1. Check if socket exists
	if _, err := os.Stat(stored.SocketPath); err != nil {
		// No socket - check for snapshot to distinguish Stopped vs Standby
		if m.hasSnapshot(stored.DataDir) {
			return stateResult{State: StateStandby}
		}
		return stateResult{State: StateStopped}
	}

	// 2. Socket exists - query VMM for actual state
	client, err := vmm.NewVMM(stored.SocketPath)
	if err != nil {
		// Failed to create client - this is unexpected if socket exists
		errMsg := fmt.Sprintf("failed to create VMM client: %v", err)
		log.WarnContext(ctx, "failed to determine instance state",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"error", err,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
	}

	resp, err := client.GetVmInfoWithResponse(ctx)
	if err != nil {
		// Socket exists but VMM is unreachable - this is unexpected
		errMsg := fmt.Sprintf("failed to query VMM: %v", err)
		log.WarnContext(ctx, "failed to query VMM state",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"error", err,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
	}

	if resp.StatusCode() != 200 || resp.JSON200 == nil {
		// VMM returned an error - log it and return Unknown
		body := string(resp.Body)
		errMsg := fmt.Sprintf("VMM returned error (status %d): %s", resp.StatusCode(), body)
		log.WarnContext(ctx, "VMM returned error response",
			"instance_id", stored.Id,
			"socket", stored.SocketPath,
			"status_code", resp.StatusCode(),
			"body", body,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
	}

	// 3. Map CH state to our state
	switch resp.JSON200.State {
	case vmm.Created:
		return stateResult{State: StateCreated}
	case vmm.Running:
		return stateResult{State: StateRunning}
	case vmm.Paused:
		return stateResult{State: StatePaused}
	case vmm.Shutdown:
		return stateResult{State: StateShutdown}
	default:
		// Unknown CH state - log and return Unknown
		errMsg := fmt.Sprintf("unexpected VMM state: %s", resp.JSON200.State)
		log.WarnContext(ctx, "VMM returned unexpected state",
			"instance_id", stored.Id,
			"vmm_state", resp.JSON200.State,
		)
		return stateResult{State: StateUnknown, Error: &errMsg}
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
	result := m.deriveState(ctx, &meta.StoredMetadata)
	inst := Instance{
		StoredMetadata: meta.StoredMetadata,
		State:          result.State,
		StateError:     result.Error,
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
