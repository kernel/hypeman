//go:build linux

package cloudhypervisor

import "github.com/kernel/hypeman/lib/hypervisor"

// Cloud Hypervisor requires KVM, so capability registration — and therefore
// its presence in hypervisor.RegisteredRuntimes — is Linux-only.
func init() {
	hypervisor.RegisterCapabilities(hypervisor.TypeCloudHypervisor, capabilities())
}
