package qemu

import (
	"fmt"
	"runtime"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// MachineType identifies a QEMU machine/board type persisted in QEMU's private
// restore contract. It is not part of the hypervisor-agnostic VM config.
type MachineType string

const (
	// MachineTypeQ35 is QEMU's standard x86 board.
	MachineTypeQ35 MachineType = "q35"
	// MachineTypeVirt is QEMU's standard ARM board.
	MachineTypeVirt MachineType = "virt"
	// MachineTypeMicroVM is QEMU's minimal x86-only board.
	MachineTypeMicroVM MachineType = "microvm"
)

// ResolveMachineType validates a QEMU machine type for this host and resolves
// an omitted type to the host architecture's standard board.
func ResolveMachineType(requested MachineType) (MachineType, error) {
	return resolveMachineTypeForPlatform(requested, runtime.GOOS, runtime.GOARCH)
}

func machineTypeForHypervisor(hypervisorType hypervisor.Type) (MachineType, error) {
	switch hypervisorType {
	case hypervisor.TypeQEMU:
		return ResolveMachineType("")
	case hypervisor.TypeQEMUMicroVM:
		return ResolveMachineType(MachineTypeMicroVM)
	default:
		return "", fmt.Errorf("unsupported QEMU hypervisor type %q", hypervisorType)
	}
}

func resolveMachineTypeForPlatform(requested MachineType, goos, goarch string) (MachineType, error) {
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

// ValidateConfig validates restrictions for standard QEMU on this host.
func ValidateConfig(cfg hypervisor.VMConfig) error {
	machine, err := ResolveMachineType("")
	if err != nil {
		return err
	}
	return validateConfig(cfg, machine)
}

// ValidateMicroVMConfig validates restrictions for QEMU's microvm profile.
func ValidateMicroVMConfig(cfg hypervisor.VMConfig) error {
	machine, err := ResolveMachineType(MachineTypeMicroVM)
	if err != nil {
		return err
	}
	return validateConfig(cfg, machine)
}

// validateConfig applies restrictions for a selected QEMU machine profile.
// It is called at launch and restore time, so snapshot configs cannot bypass
// create-time request validation.
func validateConfig(cfg hypervisor.VMConfig, machine MachineType) error {
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
