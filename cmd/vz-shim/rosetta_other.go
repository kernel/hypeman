//go:build darwin && !arm64

package main

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// configureDirectorySharing is the non-arm64 counterpart of the arm64
// implementation: Rosetta does not exist on Intel macOS, so it errors when
// requested and is otherwise a no-op. This mirrors the existing
// save_restore_unsupported.go stub. In practice the whole package is arm64-only
// because Code-Hex/vz v3.7.1 does not compile for darwin/amd64, so this file
// only satisfies the type checker for the !arm64 build tag.
func configureDirectorySharing(_ *vz.VirtualMachineConfiguration, config *shimconfig.ShimConfig) error {
	if config.EnableRosetta {
		return fmt.Errorf("rosetta is only available on Apple silicon")
	}
	return nil
}
