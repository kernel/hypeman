package instances

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/resources"
)

// ListInstanceAllocations returns a conservative cached allocation view used by
// both admission control and resource status reporting. The cache is initialized
// once from disk, then kept in sync by instance lifecycle metadata updates.
func (m *manager) ListInstanceAllocations(ctx context.Context) ([]resources.InstanceAllocation, error) {
	if err := m.ensureAdmissionAllocations(); err != nil {
		return nil, err
	}

	m.admissionAllocationsMu.RLock()
	defer m.admissionAllocationsMu.RUnlock()

	allocations := make([]resources.InstanceAllocation, 0, len(m.admissionAllocations))
	for _, allocation := range m.admissionAllocations {
		allocations = append(allocations, allocation)
	}

	return allocations, nil
}

func (m *manager) ensureAdmissionAllocations() error {
	m.admissionAllocationsMu.RLock()
	if m.admissionAllocationsLoaded {
		m.admissionAllocationsMu.RUnlock()
		return nil
	}
	m.admissionAllocationsMu.RUnlock()

	metaFiles, err := m.listMetadataFiles()
	if err != nil {
		return err
	}

	allocations := make(map[string]resources.InstanceAllocation, len(metaFiles))
	for _, metaFile := range metaFiles {
		id := filepath.Base(filepath.Dir(metaFile))
		meta, err := m.loadMetadata(id)
		if err != nil {
			continue
		}
		// Cold-start cache hydration uses socket existence as the ground-truth
		// signal for "currently active" because persisted metadata can outlive a
		// crashed or already-stopped hypervisor process.
		allocations[id] = m.allocationFromStoredMetadata(&meta.StoredMetadata, admissionSocketActive(&meta.StoredMetadata))
	}

	m.admissionAllocationsMu.Lock()
	defer m.admissionAllocationsMu.Unlock()
	if !m.admissionAllocationsLoaded {
		m.admissionAllocations = allocations
		m.admissionAllocationsLoaded = true
	}

	return nil
}

func (m *manager) syncAdmissionAllocation(meta *metadata) {
	allocation := m.allocationFromStoredMetadata(&meta.StoredMetadata, admissionMetadataActive(&meta.StoredMetadata))

	m.admissionAllocationsMu.Lock()
	defer m.admissionAllocationsMu.Unlock()

	if !m.admissionAllocationsLoaded {
		return
	}
	if m.admissionAllocations == nil {
		m.admissionAllocations = make(map[string]resources.InstanceAllocation)
	}

	// Incremental cache updates trust metadata because lifecycle paths update
	// visibility explicitly once boot/restore has succeeded, avoiding repeated
	// filesystem/socket probes on the hot path.
	m.admissionAllocations[meta.Id] = allocation
}

func (m *manager) setAdmissionAllocationActive(stored *StoredMetadata, active bool) {
	allocation := m.allocationFromStoredMetadata(stored, active)

	m.admissionAllocationsMu.Lock()
	defer m.admissionAllocationsMu.Unlock()

	if !m.admissionAllocationsLoaded {
		return
	}
	if m.admissionAllocations == nil {
		m.admissionAllocations = make(map[string]resources.InstanceAllocation)
	}

	m.admissionAllocations[stored.Id] = allocation
}

func (m *manager) rollbackAdmissionAllocationActive(stored *StoredMetadata) {
	if stored == nil {
		return
	}
	// Failed post-boot/restore steps should not leave the cached visible
	// allocation marked active. Clear the in-memory PID first so any later sync
	// from this metadata view also treats the instance as inactive.
	stored.HypervisorPID = nil
	stored.HypervisorStartTime = 0
	m.setAdmissionAllocationActive(stored, false)
}

func (m *manager) deleteAdmissionAllocation(id string) {
	m.admissionAllocationsMu.Lock()
	defer m.admissionAllocationsMu.Unlock()

	if !m.admissionAllocationsLoaded || m.admissionAllocations == nil {
		return
	}
	delete(m.admissionAllocations, id)
}

func (m *manager) allocationFromStoredMetadata(stored *StoredMetadata, active bool) resources.InstanceAllocation {
	var volumeOverlayBytes int64
	var volumeBytes int64
	for _, vol := range stored.Volumes {
		if vol.Overlay {
			volumeOverlayBytes += vol.OverlaySize
		}
		if m.volumeManager != nil {
			if volume, err := m.volumeManager.GetVolume(context.Background(), vol.VolumeID); err == nil {
				volumeBytes += int64(volume.SizeGb) * 1024 * 1024 * 1024
			}
		}
	}

	state := string(StateStopped)
	if active {
		state = string(StateCreated)
	}

	return resources.InstanceAllocation{
		ID:                 stored.Id,
		Name:               stored.Name,
		Vcpus:              stored.Vcpus,
		MemoryBytes:        stored.Size + stored.HotplugSize,
		OverlayBytes:       stored.OverlaySize,
		VolumeOverlayBytes: volumeOverlayBytes,
		NetworkDownloadBps: stored.NetworkBandwidthDownload,
		NetworkUploadBps:   stored.NetworkBandwidthUpload,
		DiskIOBps:          stored.DiskIOBps,
		State:              state,
		VolumeBytes:        volumeBytes,
	}
}

func admissionMetadataActive(stored *StoredMetadata) bool {
	return stored.HypervisorPID != nil
}

func admissionSocketActive(stored *StoredMetadata) bool {
	if stored.SocketPath == "" {
		return false
	}
	_, err := os.Stat(stored.SocketPath)
	return err == nil
}

func (m *manager) StartAdmissionAllocationReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.admissionReconcileOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.reconcileAdmissionAllocations(ctx)
				}
			}
		}()
	})
}

func (m *manager) reconcileAdmissionAllocations(ctx context.Context) {
	if err := m.ensureAdmissionAllocations(); err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to ensure admission allocations before reconcile", "error", err)
		return
	}

	activeIDs := m.activeAdmissionAllocationIDs()
	for _, id := range activeIDs {
		meta, err := m.loadMetadata(id)
		if err != nil {
			m.deleteAdmissionAllocation(id)
			continue
		}

		active := admissionSocketActive(&meta.StoredMetadata)
		allocation := m.allocationFromStoredMetadata(&meta.StoredMetadata, active)

		m.admissionAllocationsMu.Lock()
		if m.admissionAllocationsLoaded && m.admissionAllocations != nil {
			m.admissionAllocations[id] = allocation
		}
		m.admissionAllocationsMu.Unlock()
	}
}

func (m *manager) activeAdmissionAllocationIDs() []string {
	m.admissionAllocationsMu.RLock()
	defer m.admissionAllocationsMu.RUnlock()

	if !m.admissionAllocationsLoaded || m.admissionAllocations == nil {
		return nil
	}

	ids := make([]string, 0, len(m.admissionAllocations))
	for id, allocation := range m.admissionAllocations {
		if isActiveAdmissionAllocationState(allocation.State) {
			ids = append(ids, id)
		}
	}
	return ids
}

func isActiveAdmissionAllocationState(state string) bool {
	switch state {
	case string(StateRunning), string(StatePaused), string(StateCreated), string(StateInitializing):
		return true
	default:
		return false
	}
}
