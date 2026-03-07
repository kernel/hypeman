//go:build linux

package main

import (
	"fmt"

	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/vmm"
)

func ensureHypervisorBinaries(p *paths.Paths) (int, string, error) {
	chBinaries := 0
	for _, version := range vmm.SupportedVersions {
		if _, err := vmm.GetBinaryPath(p, version); err != nil {
			return 0, "", fmt.Errorf("cloud-hypervisor %s: %w", version, err)
		}
		chBinaries++
	}

	fcPath, err := firecracker.NewStarter().GetBinaryPath(p, "")
	if err != nil {
		return 0, "", fmt.Errorf("firecracker: %w", err)
	}

	return chBinaries, fcPath, nil
}
