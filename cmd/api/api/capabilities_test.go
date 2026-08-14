package api

import (
	"context"
	"runtime"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

// stubCapabilitiesNetworkManager overrides EffectiveDefaultNetwork with a fixed
// network so the handler test is hermetic (no bridge/netlink).
type stubCapabilitiesNetworkManager struct {
	network.Manager
	nw  *network.Network
	err error
}

func (s *stubCapabilitiesNetworkManager) EffectiveDefaultNetwork() (*network.Network, error) {
	return s.nw, s.err
}

func cloudHypervisorCaps(t *testing.T) hypervisor.Capabilities {
	t.Helper()
	caps, ok := hypervisor.CapabilitiesForType(hypervisor.TypeCloudHypervisor)
	require.True(t, ok, "cloud-hypervisor capabilities must be registered")
	return caps
}

func TestGetCapabilities(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	svc.Config.Version = "testsha123"
	svc.NetworkManager = &stubCapabilitiesNetworkManager{
		nw: &network.Network{
			Name:     "default",
			Subnet:   "10.100.0.0/16",
			Gateway:  "10.100.0.1",
			Isolated: true,
			Default:  true,
		},
	}

	resp, err := svc.GetCapabilities(ctx(), oapi.GetCapabilitiesRequestObject{})
	require.NoError(t, err)

	okResp, ok := resp.(oapi.GetCapabilities200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	caps := oapi.Capabilities(okResp)

	// Server identity and version serialization
	require.Equal(t, "testsha123", caps.Server.Version)
	require.Equal(t, apiVersion(), caps.Server.ApiVersion)
	require.NotEmpty(t, caps.Server.ApiVersion)
	require.NotEqual(t, "unknown", caps.Server.ApiVersion)

	// Host identity
	require.Equal(t, runtime.GOOS, caps.Host.Os)
	require.Equal(t, runtime.GOARCH, caps.Host.Arch)

	// Effective default runtime and its capabilities. The test-service manager
	// defaults to cloud-hypervisor; on macOS that default cannot run, so the
	// endpoint must report zeroed runtime features rather than overstate.
	require.Equal(t, string(hypervisor.TypeCloudHypervisor), caps.Runtime.Default)
	require.NotEmpty(t, caps.Runtime.Supported)
	if runtime.GOOS == "darwin" {
		require.Equal(t, []string{string(hypervisor.TypeVZ)}, caps.Runtime.Supported)
		require.False(t, caps.Runtime.Standby, "unusable default runtime must not advertise standby")
		require.False(t, caps.Runtime.Snapshot)
	} else {
		require.Contains(t, caps.Runtime.Supported, caps.Runtime.Default)
		chCaps := cloudHypervisorCaps(t)
		require.Equal(t, chCaps.SupportsSnapshot, caps.Runtime.Snapshot)
		require.Equal(t, chCaps.SupportsPause, caps.Runtime.Pause)
		require.True(t, caps.Runtime.Standby)
	}

	// Network model and guest-visible gateway
	require.Equal(t, oapi.CapabilitiesNetworkModel(network.NetworkModel()), caps.Network.Model)
	require.Equal(t, "10.100.0.1", caps.Network.Gateway)
	require.NotNil(t, caps.Network.Subnet)
	require.Equal(t, "10.100.0.0/16", *caps.Network.Subnet)
	require.False(t, caps.Network.GuestToGuest, "isolated default network must report guest_to_guest=false")

	// Image platforms always include the host-native guest platform
	require.Contains(t, caps.Images.Platforms, caps.Images.DefaultPlatform)

	// Base feature IDs are always present
	for _, f := range []string{"instances", "images", "builds", "volumes", "ingress", "exec", "logs"} {
		require.Contains(t, caps.Features, f)
	}
	if runtime.GOOS == "darwin" {
		require.NotContains(t, caps.Features, "devices")
		require.NotContains(t, caps.Features, "standby")
	} else {
		require.Contains(t, caps.Features, "standby")
		require.Contains(t, caps.Features, "devices")
	}
}

func TestGetCapabilitiesNetworkError(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	svc.NetworkManager = &stubCapabilitiesNetworkManager{err: context.DeadlineExceeded}

	resp, err := svc.GetCapabilities(ctx(), oapi.GetCapabilitiesRequestObject{})
	require.NoError(t, err)
	_, ok := resp.(oapi.GetCapabilities500JSONResponse)
	require.True(t, ok, "expected 500 response when network resolution fails, got %T", resp)
}

func TestSupportedRuntimes(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		[]string{"cloud-hypervisor", "firecracker", "qemu"},
		supportedRuntimes("linux"))
	require.Equal(t, []string{"vz"}, supportedRuntimes("darwin"))
}

func TestEmulationSupported(t *testing.T) {
	t.Parallel()
	require.True(t, emulationSupported("darwin", "arm64", []string{"vz"}))
	require.False(t, emulationSupported("darwin", "arm64", []string{"cloud-hypervisor"}))
	require.False(t, emulationSupported("darwin", "amd64", []string{"vz"}))
	require.False(t, emulationSupported("linux", "arm64", []string{"vz"}))
	require.False(t, emulationSupported("linux", "amd64", []string{"cloud-hypervisor"}))
}

func TestImagePlatforms(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"linux/amd64"}, imagePlatforms("amd64", false))
	require.Equal(t, []string{"linux/arm64"}, imagePlatforms("arm64", false))
	require.Equal(t,
		[]string{"linux/arm64", "linux/amd64"},
		imagePlatforms("arm64", true),
		"Apple Silicon macOS with vz advertises Rosetta-emulated amd64")
}

func TestAssembleFeatures(t *testing.T) {
	t.Parallel()

	base := []string{"instances", "images", "builds", "volumes", "ingress", "exec", "logs"}

	// Zero capabilities (unknown runtime on this host): base features only.
	features := assembleFeatures("linux", hypervisor.Capabilities{}, false)
	require.ElementsMatch(t, append(base, "devices"), features)

	// Full Linux cloud-hypervisor capabilities.
	features = assembleFeatures("linux", cloudHypervisorCaps(t), false)
	require.Subset(t, features, base)
	require.Contains(t, features, "standby")
	require.Contains(t, features, "snapshots")
	require.Contains(t, features, "fork")
	require.Contains(t, features, "pause")
	require.Contains(t, features, "hotplug-memory")
	require.Contains(t, features, "devices")
	require.NotContains(t, features, "rosetta-emulation")

	// macOS: no devices feature; emulation adds rosetta.
	vzCaps := hypervisor.Capabilities{SupportsSnapshot: true, SupportsPause: true, SupportsBalloonControl: true, SupportsVsock: true}
	features = assembleFeatures("darwin", vzCaps, true)
	require.NotContains(t, features, "devices")
	require.Contains(t, features, "standby")
	require.Contains(t, features, "rosetta-emulation")
	require.NotContains(t, features, "hotplug-memory")
	require.NotContains(t, features, "gpu-passthrough")
}

func TestStandbyRequiresSnapshotAndPause(t *testing.T) {
	t.Parallel()
	require.False(t, standbySupported(hypervisor.Capabilities{SupportsSnapshot: true, SupportsPause: false}))
	require.False(t, standbySupported(hypervisor.Capabilities{SupportsSnapshot: false, SupportsPause: true}))
	require.True(t, standbySupported(hypervisor.Capabilities{SupportsSnapshot: true, SupportsPause: true}))
}

// TestAPIVersionMatchesSpec guards the version contract: the endpoint must
// serialize the same version as the embedded OpenAPI document.
func TestAPIVersionMatchesSpec(t *testing.T) {
	t.Parallel()
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	require.Equal(t, spec.Info.Version, apiVersion())
}
