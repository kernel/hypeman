//go:build linux

package cloudhypervisor

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/require"
)

// TestRegisteredCapabilitiesTrackConfiguredDefaultVersion pins the v49/v51
// boundary: the capability registry must resolve cloud-hypervisor
// capabilities from the configured default version at read time, not from a
// value frozen at package init. v49.0 lacks live disk resize; advertising it
// would misreport what ordinary configured launches can do.
//
// Not parallel: it mutates the process-wide configured default version.
func TestRegisteredCapabilitiesTrackConfiguredDefaultVersion(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, SetDefaultVersion("")) })

	require.NoError(t, SetDefaultVersion(string(vmm.V49_0)))
	caps, ok := hypervisor.CapabilitiesForType(hypervisor.TypeCloudHypervisor)
	require.True(t, ok, "cloud-hypervisor must be registered on Linux")
	require.False(t, caps.SupportsDiskResize, "a v49.0 default must not advertise disk-resize")
	require.NotContains(t, caps.FeatureIDs(), hypervisor.FeatureDiskResize)

	require.NoError(t, SetDefaultVersion(string(vmm.V51_1)))
	caps, ok = hypervisor.CapabilitiesForType(hypervisor.TypeCloudHypervisor)
	require.True(t, ok)
	require.True(t, caps.SupportsDiskResize, "a v51.1 default must advertise disk-resize")
	require.Contains(t, caps.FeatureIDs(), hypervisor.FeatureDiskResize)

	// The registry enumeration used by the capabilities endpoint must agree
	// with the per-type lookup.
	for _, rt := range hypervisor.RegisteredRuntimes() {
		if rt.Type == hypervisor.TypeCloudHypervisor {
			require.True(t, rt.Capabilities.SupportsDiskResize)
			require.NoError(t, rt.LaunchErr, "cloud-hypervisor binaries are embedded; no launch prerequisites")
		}
	}
}

// TestCapabilitiesAdvertiseForkOnEveryVersion pins that fork support is
// explicit and version-independent for cloud-hypervisor, matching the
// PrepareFork implementation in fork.go.
func TestCapabilitiesAdvertiseForkOnEveryVersion(t *testing.T) {
	t.Parallel()
	for _, v := range vmm.SupportedVersions {
		require.True(t, CapabilitiesForVersion(v).SupportsFork, "version %s", v)
	}
}

// TestCapabilitiesAdvertiseDiskResizeOnV51 pins live disk resize for every
// v51 release, including the v51.2 default used for new instances.
func TestCapabilitiesAdvertiseDiskResizeOnV51(t *testing.T) {
	t.Parallel()
	require.Equal(t, vmm.V51_2, vmm.DefaultVersion)
	for _, v := range []vmm.CHVersion{vmm.V51_1, vmm.V51_2} {
		require.True(t, CapabilitiesForVersion(v).SupportsDiskResize, "version %s", v)
	}
}
