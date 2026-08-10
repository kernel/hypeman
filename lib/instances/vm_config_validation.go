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

// validateStoredVMConfig applies the same backend validation before a stopped
// snapshot is restored or forked onto a target hypervisor.
func (m *manager) validateStoredVMConfig(starter hypervisor.VMStarter, meta StoredMetadata, hvType hypervisor.Type) error {
	pciDeviceCount := len(meta.Devices)
	if meta.GPUMdevUUID != "" || meta.GPUProfile != "" {
		pciDeviceCount++
	}
	return validatePlannedVMConfig(starter, hvType, m.plannedVMConfig(
		meta.HotplugSize,
		meta.Volumes,
		meta.NetworkEnabled,
		pciDeviceCount,
	))
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
