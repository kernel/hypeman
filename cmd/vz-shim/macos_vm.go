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
	if err := configureMacAudio(vmConfig); err != nil {
		return nil, nil, fmt.Errorf("configure audio: %w", err)
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

// configureMacAudio gives the guest an audio device. Virtualization attaches audio
// only through a sound device, so without one the guest boots with no CoreAudio
// devices at all: `system_profiler SPAudioDataType` lists none and `afplay` fails
// with `AudioQueueStart failed (-66680)`.
//
// The output stream has no host sink. The samples are discarded on the host, which
// is what a headless browser VM wants -- nothing should play on the machine running
// the guests -- while the guest still has a real output device, so applications can
// render audio and ScreenCaptureKit has a mix to capture for the live view.
func configureMacAudio(vmConfig *vz.VirtualMachineConfiguration) error {
	soundConfig, err := vz.NewVirtioSoundDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("create sound device: %w", err)
	}
	output, err := vz.NewVirtioSoundDeviceOutputStreamConfiguration()
	if err != nil {
		return fmt.Errorf("create output stream: %w", err)
	}
	soundConfig.SetStreams(output)
	vmConfig.SetAudioDevicesVirtualMachineConfiguration([]vz.AudioDeviceConfiguration{soundConfig})
	return nil
}
