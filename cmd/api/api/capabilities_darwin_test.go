//go:build darwin

package api

import (
	"runtime"
	"slices"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

// TestGetCapabilitiesRosettaTracksProbe pins that Rosetta emulation reporting
// follows the live Virtualization.framework availability probe on this host
// — the same check the vz-shim enforces at launch — rather than treating
// every Apple Silicon host as emulation-capable: a host without Rosetta
// installed (or on macOS < 13) must advertise neither the rosetta-emulation
// feature nor the linux/amd64 image platform.
func TestGetCapabilitiesRosettaTracksProbe(t *testing.T) {
	t.Parallel()
	if runtime.GOARCH != "arm64" {
		t.Skipf("rosetta emulation exists only on Apple Silicon (GOARCH=%s)", runtime.GOARCH)
	}
	svc := newTestService(t)
	svc.NetworkManager = &stubCapabilitiesNetworkManager{}

	caps := getCapabilities(t, svc)

	want := rosettaInstalled()
	require.Equal(t, want, slices.Contains(caps.Features, "rosetta-emulation"),
		"rosetta-emulation feature must track the launch-path availability probe")
	require.Equal(t, want, slices.Contains(caps.Images.Platforms, "linux/amd64"),
		"linux/amd64 image platform must track the launch-path availability probe")
}

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
