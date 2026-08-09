package qemu

import (
	"fmt"
	"runtime"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const (
	// MachineTypeQ35 is QEMU's standard x86 board.
	MachineTypeQ35 hypervisor.MachineType = "q35"
	// MachineTypeVirt is QEMU's standard ARM board.
	MachineTypeVirt hypervisor.MachineType = "virt"
	// MachineTypeMicroVM is QEMU's minimal x86-only board.
	MachineTypeMicroVM hypervisor.MachineType = "microvm"
)

// ResolveMachineType validates a QEMU machine type for this host and resolves
// an omitted type to the host architecture's standard board.
func ResolveMachineType(requested hypervisor.MachineType) (hypervisor.MachineType, error) {
	return resolveMachineTypeForPlatform(requested, runtime.GOOS, runtime.GOARCH)
}

func resolveMachineTypeForPlatform(requested hypervisor.MachineType, goos, goarch string) (hypervisor.MachineType, error) {
	switch goarch {
	case "amd64":
		if requested == "" {
			return MachineTypeQ35, nil
		}
		if requested == MachineTypeQ35 {
			return requested, nil
		}
		if requested == MachineTypeMicroVM && goos == "linux" {
			return requested, nil
		}
	case "arm64":
		if requested == "" || requested == MachineTypeVirt {
			return MachineTypeVirt, nil
		}
	default:
		return "", fmt.Errorf("unsupported platform %s/%s", goos, goarch)
	}

	return "", fmt.Errorf("machine type %q is not supported on %s/%s", requested, goos, goarch)
}

// ValidateConfig validates the QEMU-specific restrictions of a VM config.
// It is called at launch and restore time, so metadata and snapshot configs
// cannot bypass create-time request validation.
func ValidateConfig(cfg hypervisor.VMConfig) error {
	machine, err := ResolveMachineType(cfg.MachineType)
	if err != nil {
		return err
	}
	if machine != MachineTypeMicroVM {
		return nil
	}
	if cfg.HotplugBytes > 0 {
		return fmt.Errorf("microvm does not support hotplug memory")
	}
	if len(cfg.PCIDevices) > 0 {
		return fmt.Errorf("microvm does not support PCI devices")
	}

	devices := len(cfg.Disks) + len(cfg.Networks)
	if cfg.VsockCID > 0 {
		devices++
	}
	if cfg.GuestMemory.EnableBalloon {
		devices++
	}
	if devices > 8 {
		return fmt.Errorf("microvm supports at most 8 virtio-mmio devices (got %d: disks=%d networks=%d vsock=%t balloon=%t)", devices, len(cfg.Disks), len(cfg.Networks), cfg.VsockCID > 0, cfg.GuestMemory.EnableBalloon)
	}
	return nil
}
