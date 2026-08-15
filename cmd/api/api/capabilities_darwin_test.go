//go:build darwin

package api

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

// TestRegisteredRuntimesDarwin pins the macOS registration boundary: only vz
// can genuinely launch VMs on macOS (the Linux backends require KVM and
// kernel AF_VSOCK), so the capability registry contains exactly vz even
// though the cloud-hypervisor and qemu packages are linked into the binary.
func TestRegisteredRuntimesDarwin(t *testing.T) {
	t.Parallel()

	registered := hypervisor.RegisteredRuntimes()
	require.Len(t, registered, 1)
	require.Equal(t, hypervisor.TypeVZ, registered[0].Type)

	for _, linuxOnly := range []hypervisor.Type{
		hypervisor.TypeCloudHypervisor,
		hypervisor.TypeFirecracker,
		hypervisor.TypeQEMU,
		hypervisor.TypeQEMUMicroVM,
	} {
		_, ok := hypervisor.CapabilitiesForType(linuxOnly)
		require.False(t, ok, "%s must not register capabilities on macOS", linuxOnly)
	}
}
