//go:build darwin && !arm64

package main

import (
	"fmt"
	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

func newMacBootLoader(*shimconfig.ShimConfig) (vz.BootLoader, error) {
	return nil, fmt.Errorf("macOS guests require Apple silicon")
}
func configureMacPlatform(*vz.VirtualMachineConfiguration, *shimconfig.ShimConfig) error {
	return fmt.Errorf("macOS guests require Apple silicon")
}
