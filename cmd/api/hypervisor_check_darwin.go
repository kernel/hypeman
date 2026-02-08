//go:build darwin

package main

import (
	"fmt"
	"runtime"

	"github.com/Code-Hex/vz/v3"
)

// checkHypervisorAccess verifies Virtualization.framework is available on macOS
func checkHypervisorAccess() error {
	// Intel Macs are intentionally unsupported: Virtualization.framework on x86_64
	// lacks nested virtualization and has critical kernel panics under load.
	if runtime.GOARCH != "arm64" {
		return fmt.Errorf("Virtualization.framework on macOS requires Apple Silicon (arm64), got %s", runtime.GOARCH)
	}

	// Validate virtualization is usable by attempting to get max CPU count
	// This will fail if entitlements are missing or virtualization is not available
	maxCPU := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	if maxCPU < 1 {
		return fmt.Errorf("Virtualization.framework reports 0 max CPUs - check entitlements")
	}

	return nil
}

// hypervisorAccessCheckName returns the name of the hypervisor access check for logging
func hypervisorAccessCheckName() string {
	return "Virtualization.framework"
}
