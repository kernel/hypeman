//go:build darwin && !arm64

package main

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// configureDirectorySharing is the non-arm64 stub: Rosetta is Apple-silicon-only,
// so it errors when requested and is otherwise a no-op. cmd/vz-shim is arm64-only
// in practice (Code-Hex/vz v3.7.1 doesn't compile for darwin/amd64); this only
// satisfies the !arm64 type check, like save_restore_unsupported.go.
func configureDirectorySharing(_ *vz.VirtualMachineConfiguration, config *shimconfig.ShimConfig) error {
	if config.EnableRosetta {
		return fmt.Errorf("rosetta is only available on Apple silicon")
	}
	return nil
}
