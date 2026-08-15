//go:build linux

package qemu

import (
	"fmt"
	"os"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// vhostVsockDevicePath is the host device QEMU's vhost-vsock backend opens.
// Every created instance is assigned a nonzero vsock CID (lib/instances), so
// buildArgs always adds a vhost-vsock device and ordinary launches fail
// without it. API startup validates KVM only, so availability must check it.
const vhostVsockDevicePath = "/dev/vhost-vsock"

// QEMU guests launch with KVM acceleration and kernel AF_VSOCK, so
// registration — and therefore presence in hypervisor.RegisteredRuntimes — is
// Linux-only. The microvm board additionally exists only for x86, so
// qemu-microvm registers only where its machine type resolves (linux/amd64);
// the same resolver rejects it at launch time.
//
// Unlike the other Linux backends, QEMU uses the host-installed system binary
// and hypeman deliberately starts without it (cmd/api/main.go only warns), so
// both QEMU registrations carry a LaunchCheck verifying the launch
// prerequisites. Registration means "supported by this build"; the check
// determines availability.
func init() {
	hypervisor.RegisterRuntime(hypervisor.TypeQEMU, hypervisor.RuntimeRegistration{
		Capabilities: StandardProfile{}.capabilities,
		LaunchCheck:  checkLaunchPrerequisites,
	})
	if _, err := microVMMachineType(); err == nil {
		hypervisor.RegisterRuntime(hypervisor.TypeQEMUMicroVM, hypervisor.RuntimeRegistration{
			Capabilities: MicroVMProfile{}.capabilities,
			LaunchCheck:  checkLaunchPrerequisites,
		})
	}
}

// checkLaunchPrerequisites is the capability registry's side-effect-free
// launch check for both QEMU boards: it resolves the system binary with the
// same lookup launches use, then verifies the prerequisites every ordinary
// launch needs. Evaluated per registry read, so installing QEMU or loading
// vhost_vsock flips availability without a restart.
func checkLaunchPrerequisites() error {
	binaryPath, err := (&Starter{}).GetBinaryPath(nil, "")
	if err != nil {
		return err
	}
	return checkLaunchPrerequisitesFor(binaryPath, vhostVsockDevicePath)
}

// checkLaunchPrerequisitesFor verifies that a resolved QEMU binary and the
// host vsock device can back an ordinary launch:
//
//   - the binary must actually execute and report a parseable version
//     (versionFromBinary), because every cold start persists ResolveVersion's
//     result and treats failure as fatal — a non-executable or broken binary
//     accepted by GetBinaryPath's bare os.Stat must not report available;
//   - the vhost-vsock host device must exist, because every created instance
//     receives a nonzero vsock CID and buildArgs unconditionally attaches a
//     vhost-vsock device for it.
//
// Split from checkLaunchPrerequisites so unavailable cases are testable with
// fake binaries and device paths regardless of the host's QEMU install.
func checkLaunchPrerequisitesFor(binaryPath, vsockDevicePath string) error {
	if err := validateExecutable(binaryPath); err != nil {
		return fmt.Errorf("qemu binary %s is not executable: %w", binaryPath, err)
	}
	if _, err := versionFromBinary(binaryPath); err != nil {
		return fmt.Errorf("qemu binary %s is not usable: %w", binaryPath, err)
	}
	if _, err := os.Stat(vsockDevicePath); err != nil {
		return fmt.Errorf("vsock device %s is required for instance launches (load the vhost_vsock kernel module): %w", vsockDevicePath, err)
	}
	return nil
}
