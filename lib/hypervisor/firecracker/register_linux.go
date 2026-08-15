//go:build linux

package firecracker

import "github.com/kernel/hypeman/lib/hypervisor"

// Firecracker requires KVM, so registration — and therefore its presence in
// hypervisor.RegisteredRuntimes — is Linux-only. No LaunchCheck: the default
// binary ships embedded in hypeman.
func init() {
	hypervisor.RegisterRuntime(hypervisor.TypeFirecracker, hypervisor.RuntimeRegistration{
		Capabilities: capabilities,
	})
}
