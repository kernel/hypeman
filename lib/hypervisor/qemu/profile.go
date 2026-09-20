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
	supportsFirmware := standardMachineType() == MachineTypeQ35
	return qemuCapabilities(true, supportsFirmware)
}
func (p StandardProfile) validateConfig(cfg hypervisor.VMConfig) error {
	if err := hypervisor.ValidateBootConfig(cfg); err != nil {
		return err
	}
	return validateProfileCapabilities(p.hypervisorType(), p.capabilities(), cfg)
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
	return qemuCapabilities(false, false)
}
func (MicroVMProfile) validateConfig(cfg hypervisor.VMConfig) error {
	if err := hypervisor.ValidateDirectRawConfig("qemu-microvm", cfg); err != nil {
		return err
	}
	if cfg.HotplugBytes > 0 {
		return fmt.Errorf("microvm does not support hotplug memory")
	}
	if len(cfg.PCIDevices) > 0 || cfg.VGPUDevicePath != "" {
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

func validateProfileCapabilities(name hypervisor.Type, caps hypervisor.Capabilities, cfg hypervisor.VMConfig) error {
	if cfg.EffectiveBootMode() == hypervisor.BootModeUEFI && !caps.SupportsUEFIBoot {
		return fmt.Errorf("%s does not support UEFI boot on this host", name)
	}
	if cfg.TPM != nil && !caps.SupportsTPM {
		return fmt.Errorf("%s does not support TPM devices", name)
	}
	return nil
}

func qemuCapabilities(supportsPCI, supportsFirmware bool) hypervisor.Capabilities {
	return hypervisor.Capabilities{
		SupportsSnapshot: true,
		// PrepareFork rewrites the saved QEMU VM config for forks (fork.go);
		// both boards share the implementation.
		SupportsFork:                true,
		SupportsHotplugMemory:       false,
		SupportsBalloonControl:      true,
		SupportsPause:               true,
		SupportsVsock:               true,
		SupportsUEFIBoot:            supportsFirmware,
		SupportsTPM:                 supportsFirmware,
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
