//go:build darwin && arm64

package main

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
)

func validateSaveRestoreSupport(vmConfig *vz.VirtualMachineConfiguration) error {
	ok, err := vmConfig.ValidateSaveRestoreSupport()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("virtual machine configuration does not support save/restore")
	}
	return nil
}

func saveMachineState(vm *vz.VirtualMachine, snapshotPath string) error {
	return vm.SaveMachineStateToPath(snapshotPath)
}

func restoreMachineState(vm *vz.VirtualMachine, snapshotPath string) error {
	// The vz wrapper accepts a filesystem path and constructs a file URL internally.
	return vm.RestoreMachineStateFromURL(snapshotPath)
}
