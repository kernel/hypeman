package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
)

const baseInstanceDiskCount = 3 // rootfs, writable overlay, and config disk

// validateQEMUMicroVMCreate validates the complete device plan before image,
// PCI, network, or filesystem side effects. The instances layer owns the disk
// plan; the QEMU package owns the microvm limits.
func (m *manager) validateQEMUMicroVMCreate(req CreateInstanceRequest, hvType hypervisor.Type) error {
	return m.validateQEMUMicroVMPlan(hvType, req.HotplugSize, req.Volumes, req.NetworkEnabled,
		len(req.Devices)+boolToInt(req.GPU != nil && req.GPU.Profile != ""))
}

// validateQEMUMicroVMMetadata applies the same policy before a stopped snapshot
// is restored or forked onto the qemu-microvm backend.
func (m *manager) validateQEMUMicroVMMetadata(meta StoredMetadata, hvType hypervisor.Type) error {
	pciDevices := len(meta.Devices)
	if meta.GPUMdevUUID != "" || meta.GPUProfile != "" {
		pciDevices++
	}
	return m.validateQEMUMicroVMPlan(hvType, meta.HotplugSize, meta.Volumes, meta.NetworkEnabled, pciDevices)
}

func (m *manager) validateQEMUMicroVMPlan(
	hvType hypervisor.Type,
	hotplugBytes int64,
	volumes []VolumeAttachment,
	networkEnabled bool,
	pciDeviceCount int,
) error {
	if hvType != hypervisor.TypeQEMUMicroVM {
		return nil
	}

	diskCount := baseInstanceDiskCount
	for _, volume := range volumes {
		diskCount++
		if volume.Overlay {
			diskCount++
		}
	}

	config := hypervisor.VMConfig{
		MachineType:  qemu.MachineTypeMicroVM,
		HotplugBytes: hotplugBytes,
		Disks:        make([]hypervisor.DiskConfig, diskCount),
		VsockCID:     3,
		GuestMemory:  m.guestMemoryConfig(),
		PCIDevices:   make([]string, pciDeviceCount),
	}
	if networkEnabled {
		config.Networks = []hypervisor.NetworkConfig{{}}
	}
	if err := qemu.ValidateConfig(config); err != nil {
		return fmt.Errorf("%w: hypervisor %s: %v", ErrInvalidRequest, hvType, err)
	}
	return nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
