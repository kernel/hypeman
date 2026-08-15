//go:build linux

package qemu

import "github.com/kernel/hypeman/lib/hypervisor"

// QEMU guests launch with KVM acceleration and kernel AF_VSOCK, so
// registration — and therefore presence in hypervisor.RegisteredRuntimes — is
// Linux-only. The microvm board additionally exists only for x86, so
// qemu-microvm registers only where its machine type resolves (linux/amd64);
// the same resolver rejects it at launch time.
//
// Unlike the other Linux backends, QEMU uses the host-installed system binary
// and hypeman deliberately starts without it (cmd/api/main.go only warns), so
// both QEMU registrations carry a LaunchCheck probing the same binary lookup
// that launches use. Registration means "supported by this build"; the check
// determines availability.
func init() {
	launchCheck := func() error {
		_, err := (&Starter{}).GetBinaryPath(nil, "")
		return err
	}
	hypervisor.RegisterRuntime(hypervisor.TypeQEMU, hypervisor.RuntimeRegistration{
		Capabilities: StandardProfile{}.capabilities,
		LaunchCheck:  launchCheck,
	})
	if _, err := microVMMachineType(); err == nil {
		hypervisor.RegisterRuntime(hypervisor.TypeQEMUMicroVM, hypervisor.RuntimeRegistration{
			Capabilities: MicroVMProfile{}.capabilities,
			LaunchCheck:  launchCheck,
		})
	}
}
