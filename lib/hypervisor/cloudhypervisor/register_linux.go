//go:build linux

package cloudhypervisor

import "github.com/kernel/hypeman/lib/hypervisor"

// Cloud Hypervisor requires KVM, so registration — and therefore its
// presence in hypervisor.RegisteredRuntimes — is Linux-only. Capabilities
// are resolved per registry read from the configured default version (which
// SetDefaultVersion may change after init, e.g. pinning v49.0 without
// disk-resize) rather than frozen from the compile-time default. No
// LaunchCheck: the binaries for every supported version ship embedded in
// hypeman.
func init() {
	hypervisor.RegisterRuntime(hypervisor.TypeCloudHypervisor, hypervisor.RuntimeRegistration{
		Capabilities: func() hypervisor.Capabilities {
			return CapabilitiesForVersion(GetDefaultVersion())
		},
	})
}
