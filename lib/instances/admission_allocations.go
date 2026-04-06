package instances

import (
	"context"
	"os"
	"path/filepath"

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
	m.admissionAllocations[meta.Id] = m.allocationFromStoredMetadata(&meta.StoredMetadata, admissionMetadataActive(&meta.StoredMetadata))
}

func (m *manager) setAdmissionAllocationActive(stored *StoredMetadata, active bool) {
	m.admissionAllocationsMu.Lock()
	defer m.admissionAllocationsMu.Unlock()

	if !m.admissionAllocationsLoaded {
		return
	}
	if m.admissionAllocations == nil {
		m.admissionAllocations = make(map[string]resources.InstanceAllocation)
	}

	m.admissionAllocations[stored.Id] = m.allocationFromStoredMetadata(stored, active)
}

func (m *manager) rollbackAdmissionAllocationActive(stored *StoredMetadata) {
	if stored == nil {
		return
	}
	// Failed post-boot/restore steps should not leave the cached visible
	// allocation marked active. Clear the in-memory PID first so any later sync
	// from this metadata view also treats the instance as inactive.
	stored.HypervisorPID = nil
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
