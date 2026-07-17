//go:build darwin && arm64

package main

import (
	"fmt"
	"log/slog"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// configureDirectorySharing attaches the Linux Rosetta virtio-fs share when
// EnableRosetta is set. The host must have Rosetta installed; NotInstalled and
// NotSupported are hard errors surfaced back through the shim-failure path.
func configureDirectorySharing(vmConfig *vz.VirtualMachineConfiguration, config *shimconfig.ShimConfig) error {
	if !config.EnableRosetta {
		return nil
	}

	switch vz.LinuxRosettaDirectoryShareAvailability() {
	case vz.LinuxRosettaAvailabilityInstalled:
	case vz.LinuxRosettaAvailabilityNotInstalled:
		return fmt.Errorf("rosetta requested but not installed (run: softwareupdate --install-rosetta)")
	default:
		return fmt.Errorf("rosetta requested but not supported on this host (requires Apple silicon + macOS 13+)")
	}

	fsDevice, err := vz.NewVirtioFileSystemDeviceConfiguration(shimconfig.RosettaMountTag)
	if err != nil {
		return fmt.Errorf("create rosetta virtio-fs device (tag %q): %w", shimconfig.RosettaMountTag, err)
	}

	share, err := vz.NewLinuxRosettaDirectoryShare()
	if err != nil {
		return fmt.Errorf("create rosetta directory share: %w", err)
	}
	fsDevice.SetDirectoryShare(share)

	vmConfig.SetDirectorySharingDevicesVirtualMachineConfiguration(
		[]vz.DirectorySharingDeviceConfiguration{fsDevice},
	)
	slog.Info("attached rosetta directory share", "tag", shimconfig.RosettaMountTag)
	return nil
}
