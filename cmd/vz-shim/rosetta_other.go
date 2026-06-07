//go:build darwin && !arm64

package main

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// configureDirectorySharing rejects Rosetta on Intel macOS, where it does not
// exist. Without EnableRosetta it is a no-op so the shim still builds for
// darwin/amd64.
func configureDirectorySharing(_ *vz.VirtualMachineConfiguration, config *shimconfig.ShimConfig) error {
	if config.EnableRosetta {
		return fmt.Errorf("rosetta is only available on Apple silicon")
	}
	return nil
}
