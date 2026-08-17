//go:build linux

package firecracker

import "github.com/kernel/hypeman/lib/hypervisor"

// Firecracker requires KVM, so registration — and therefore its presence in
// hypervisor.RegisteredRuntimes — is Linux-only. The default binary ships
// embedded in hypeman, but hypervisor.firecracker_binary_path overrides it
// on every launch, so the LaunchCheck validates the active override (if any)
// with the same executable check the launch path applies.
func init() {
	hypervisor.RegisterRuntime(hypervisor.TypeFirecracker, hypervisor.RuntimeRegistration{
		Capabilities: capabilities,
		LaunchCheck:  checkLaunchPrerequisites,
	})
}
