//go:build darwin

package main

import (
	"fmt"
	"log/slog"

	"github.com/Code-Hex/vz/v4"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

func isMacGuest(config *shimconfig.ShimConfig) bool {
	return config.MacHardwareModelData != "" || config.MacMachineIdentifierData != "" || config.MacAuxStoragePath != ""
}

// createMacVM boots an imported macOS disk with its preserved platform identity.
// It has no serial console, entropy device, Rosetta share or memory balloon.
func createMacVM(config *shimconfig.ShimConfig) (*vz.VirtualMachine, *vz.VirtualMachineConfiguration, error) {
	if config.EnableRosetta || config.EnableMemoryBalloon || config.RequireMemoryBalloon {
		return nil, nil, fmt.Errorf("macOS guests do not support Linux Rosetta or memory ballooning")
	}
	bootLoader, err := newMacBootLoader(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create boot loader: %w", err)
	}

	vcpus := uint(config.VCPUs)
	memoryBytes := uint64(config.MemoryBytes)
	slog.Debug("macOS VM config", "vcpus", vcpus, "memory_bytes", memoryBytes)

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, vcpus, memoryBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("create vm configuration: %w", err)
	}
	if err := configureNetwork(vmConfig, config.Networks); err != nil {
		return nil, nil, fmt.Errorf("configure network: %w", err)
	}
	if err := configureStorage(vmConfig, config.Disks); err != nil {
		return nil, nil, fmt.Errorf("configure storage: %w", err)
	}
	if err := configureDirectorySharing(vmConfig, config); err != nil {
		return nil, nil, fmt.Errorf("configure directory sharing: %w", err)
	}
	if err := configureMacPlatform(vmConfig, config); err != nil {
		return nil, nil, fmt.Errorf("configure Mac platform: %w", err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("create vsock device: %w", err)
	}
	vmConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	validated, err := vmConfig.Validate()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid vm configuration: %w", err)
	}
	if !validated {
		return nil, nil, fmt.Errorf("invalid vm configuration")
	}
	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("create virtual machine: %w", err)
	}
	return vm, vmConfig, nil
}
