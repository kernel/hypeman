//go:build linux

package qemu

import "github.com/kernel/hypeman/lib/hypervisor"

// QEMU guests launch with KVM acceleration and kernel AF_VSOCK, so capability
// registration — and therefore presence in hypervisor.RegisteredRuntimes — is
// Linux-only. The microvm board additionally exists only for x86, so
// qemu-microvm registers only where its machine type resolves (linux/amd64);
// the same resolver rejects it at launch time.
func init() {
	hypervisor.RegisterCapabilities(hypervisor.TypeQEMU, StandardProfile{}.capabilities())
	if _, err := microVMMachineType(); err == nil {
		hypervisor.RegisterCapabilities(hypervisor.TypeQEMUMicroVM, MicroVMProfile{}.capabilities())
	}
}
