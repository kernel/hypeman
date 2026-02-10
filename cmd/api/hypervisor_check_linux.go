//go:build linux

package main

import (
	"fmt"
	"os"
)

// checkHypervisorAccess verifies KVM is available and the user has permission to use it
func checkHypervisorAccess() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("/dev/kvm not found - KVM not enabled or not supported")
		}
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied accessing /dev/kvm - user not in 'kvm' group")
		}
		return fmt.Errorf("cannot access /dev/kvm: %w", err)
	}
	f.Close()
	return nil
}

// hypervisorAccessCheckName returns the name of the hypervisor access check for logging
func hypervisorAccessCheckName() string {
	return "KVM"
}
