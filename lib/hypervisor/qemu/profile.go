package qemu

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// profile defines the backend-specific policy layered over the shared QEMU
// process, QMP, snapshot, and restore implementation.
type profile interface {
	hypervisorType() hypervisor.Type
	machineType() (MachineType, error)
	capabilities() hypervisor.Capabilities
	validateConfig(hypervisor.VMConfig) error
	requiresStoredMachineType() bool
	requiresStoredVersion() bool
}

// StandardProfile selects QEMU's architecture-native general-purpose board.
type StandardProfile struct{}

func (StandardProfile) hypervisorType() hypervisor.Type { return hypervisor.TypeQEMU }
func (StandardProfile) machineType() (MachineType, error) {
	return standardMachineType(), nil
}
func (StandardProfile) capabilities() hypervisor.Capabilities {
	return qemuCapabilities(true)
}
func (StandardProfile) validateConfig(cfg hypervisor.VMConfig) error {
	if err := hypervisor.ValidateBootConfig(cfg); err != nil {
		return err
	}
	if cfg.EffectiveBootMode() == hypervisor.BootModeUEFI && standardMachineType() != MachineTypeQ35 {
		return fmt.Errorf("UEFI boot is currently supported only by qemu/q35 on amd64")
	}
	return nil
}
func (StandardProfile) requiresStoredMachineType() bool { return false }
func (StandardProfile) requiresStoredVersion() bool     { return false }

// MicroVMProfile selects QEMU's minimal x86 microvm board and enforces its
// virtio-mmio device contract.
type MicroVMProfile struct{}

func (MicroVMProfile) hypervisorType() hypervisor.Type { return hypervisor.TypeQEMUMicroVM }
func (MicroVMProfile) machineType() (MachineType, error) {
	return microVMMachineType()
}
func (MicroVMProfile) capabilities() hypervisor.Capabilities {
	return qemuCapabilities(false)
}
func (MicroVMProfile) validateConfig(cfg hypervisor.VMConfig) error {
	if err := hypervisor.ValidateDirectRawConfig("qemu-microvm", cfg); err != nil {
		return err
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
func (MicroVMProfile) requiresStoredMachineType() bool { return true }
func (MicroVMProfile) requiresStoredVersion() bool     { return true }

func qemuCapabilities(supportsPCI bool) hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot: true,
		// PrepareFork rewrites the saved QEMU VM config for forks (fork.go);
		// both boards share the implementation.
		SupportsFork:                true,
		SupportsHotplugMemory:       false,
		SupportsBalloonControl:      true,
		SupportsPause:               true,
		SupportsVsock:               true,
		SupportsGPUPassthrough:      supportsPCI,
		SupportsDiskIOLimit:         true,
		SupportsGracefulVMMShutdown: true,
		SupportsSnapshotBaseReuse:   false,
		// Both profiles use the single host-installed QEMU binary and an
		// unversioned machine alias. Restoring with a different QEMU version is
		// not a compatibility contract Hypeman can safely provide.
		RequiresHostSnapshotVersion: true,
	}
}

func profileForType(hypervisorType hypervisor.Type) (profile, error) {
	switch hypervisorType {
	case hypervisor.TypeQEMU:
		return StandardProfile{}, nil
	case hypervisor.TypeQEMUMicroVM:
		return MicroVMProfile{}, nil
	default:
		return nil, fmt.Errorf("unsupported QEMU hypervisor type %q", hypervisorType)
	}
}

func mustProfileForType(hypervisorType hypervisor.Type) profile {
	profile, err := profileForType(hypervisorType)
	if err != nil {
		panic(err)
	}
	return profile
}
