//go:build linux

package firecracker

import "github.com/kernel/hypeman/lib/hypervisor"

// Firecracker requires KVM, so capability registration — and therefore its
// presence in hypervisor.RegisteredRuntimes — is Linux-only.
func init() {
	hypervisor.RegisterCapabilities(hypervisor.TypeFirecracker, capabilities())
}
