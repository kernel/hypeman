//go:build darwin && !arm64

package main

import (
	"fmt"
	"runtime"

	"github.com/Code-Hex/vz/v3"
)

func validateSaveRestoreSupport(vmConfig *vz.VirtualMachineConfiguration) error {
	return fmt.Errorf("save/restore is only supported on darwin/arm64 (current arch: %s)", runtime.GOARCH)
}

func saveMachineState(vm *vz.VirtualMachine, snapshotPath string) error {
	return fmt.Errorf("save/restore is only supported on darwin/arm64 (current arch: %s)", runtime.GOARCH)
}

func restoreMachineState(vm *vz.VirtualMachine, snapshotPath string) error {
	return fmt.Errorf("save/restore is only supported on darwin/arm64 (current arch: %s)", runtime.GOARCH)
}
