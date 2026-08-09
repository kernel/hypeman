package instances

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
)

// resolveCreateMachineType validates a QEMU backend profile before image,
// device, or network side effects and returns the internal QEMU board persisted
// in VM metadata. Machine types are not part of the public API.
func (m *manager) resolveCreateMachineType(req CreateInstanceRequest, hvType hypervisor.Type) (hypervisor.MachineType, error) {
	switch hvType {
	case hypervisor.TypeQEMU:
		machine, err := qemu.ResolveMachineType("")
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		return machine, nil
	case hypervisor.TypeQEMUMicroVM:
		machine, err := qemu.ResolveMachineType(qemu.MachineTypeMicroVM)
		if err != nil {
			return "", fmt.Errorf("%w: hypervisor %s: %v", ErrInvalidRequest, hvType, err)
		}
		if req.HotplugSize > 0 {
			return "", fmt.Errorf("%w: hypervisor %s does not support hotplug memory", ErrInvalidRequest, hvType)
		}
		if len(req.Devices) > 0 {
			return "", fmt.Errorf("%w: hypervisor %s does not support PCI devices", ErrInvalidRequest, hvType)
		}
		if req.GPU != nil && req.GPU.Profile != "" {
			return "", fmt.Errorf("%w: hypervisor %s does not support vGPU devices", ErrInvalidRequest, hvType)
		}

		disks := 3
		for _, volume := range req.Volumes {
			disks++
			if volume.Overlay {
				disks++
			}
		}
		devices := disks + 1 // vsock
		if req.NetworkEnabled {
			devices++
		}
		balloon := m.guestMemoryConfig().EnableBalloon
		if balloon {
			devices++
		}
		if devices > 8 {
			return "", fmt.Errorf("%w: hypervisor %s supports at most 8 virtio-mmio devices (got %d: disks=%d networks=%d vsock=1 balloon=%t)", ErrInvalidRequest, hvType, devices, disks, boolToInt(req.NetworkEnabled), balloon)
		}
		return machine, nil
	default:
		return "", nil
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
