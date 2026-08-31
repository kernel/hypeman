package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const baseInstanceDiskCount = 3 // rootfs, writable overlay, and config disk

func instanceDiskCount(volumes []VolumeAttachment) int {
	count := baseInstanceDiskCount
	for _, volume := range volumes {
		count++
		if volume.Overlay {
			count++
		}
	}
	return count
}

// validateCreateVMConfig performs side-effect-free backend validation against
// the complete device plan before image, PCI, network, or filesystem work.
func (m *manager) validateCreateVMConfig(starter hypervisor.VMStarter, req CreateInstanceRequest, hvType hypervisor.Type) error {
	pciDeviceCount := len(req.Devices)
	if req.GPU != nil && req.GPU.Profile != "" {
		pciDeviceCount++
	}
	return validatePlannedVMConfig(starter, hvType, m.plannedVMConfig(
		req.HotplugSize,
		req.Volumes,
		req.NetworkEnabled,
		pciDeviceCount,
	))
}

// validateStoredVMConfig applies backend validation before a snapshot is
// restored or forked onto a target hypervisor.
func (m *manager) validateStoredVMConfig(starter hypervisor.VMStarter, snapshotKind SnapshotKind, meta StoredMetadata, hvType hypervisor.Type) error {
	return validatePlannedVMConfig(starter, hvType, m.plannedStoredVMConfig(snapshotKind, meta))
}

func (m *manager) plannedStoredVMConfig(snapshotKind SnapshotKind, meta StoredMetadata) hypervisor.VMConfig {
	pciDeviceCount := len(meta.Devices)
	if meta.GPUMdevUUID != "" || meta.GPUProfile != "" {
		pciDeviceCount++
	}
	config := m.plannedVMConfig(
		meta.HotplugSize,
		meta.Volumes,
		meta.NetworkEnabled,
		pciDeviceCount,
	)
	if snapshotKind == SnapshotKindStandby {
		// Standby restore/fork reuses the frozen snapshot device model, so live
		// host balloon policy must not change its preflight device count.
		config.GuestMemory = hypervisor.GuestMemoryConfig{}
	}
	return config
}

func (m *manager) plannedVMConfig(
	hotplugBytes int64,
	volumes []VolumeAttachment,
	networkEnabled bool,
	pciDeviceCount int,
) hypervisor.VMConfig {
	diskCount := instanceDiskCount(volumes)

	config := hypervisor.VMConfig{
		HotplugBytes: hotplugBytes,
		Disks:        make([]hypervisor.DiskConfig, diskCount),
		VsockCID:     3,
		GuestMemory:  m.guestMemoryConfig(),
		PCIDevices:   make([]string, pciDeviceCount),
	}
	if networkEnabled {
		config.Networks = []hypervisor.NetworkConfig{{}}
	}
	return config
}

func validatePlannedVMConfig(starter hypervisor.VMStarter, hvType hypervisor.Type, config hypervisor.VMConfig) error {
	if err := starter.ValidateConfig(config); err != nil {
		return fmt.Errorf("%w: hypervisor %s: %v", ErrInvalidRequest, hvType, err)
	}
	return nil
}
