package api

import (
	"context"
	"runtime"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
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

// stubDefaultRuntimeInstanceManager overrides the configured default runtime
// via the optional defaultHypervisorProvider accessor.
type stubDefaultRuntimeInstanceManager struct {
	instances.Manager
	defaultRuntime hypervisor.Type
}

func (s *stubDefaultRuntimeInstanceManager) DefaultHypervisor() hypervisor.Type {
	return s.defaultRuntime
}

// stubOpaqueInstanceManager implements instances.Manager without the optional
// DefaultHypervisor accessor, standing in for alternate Manager
// implementations compiled against the public module.
type stubOpaqueInstanceManager struct {
	instances.Manager
}

func getCapabilities(t *testing.T, svc *ApiService) oapi.Capabilities {
	t.Helper()
	resp, err := svc.GetCapabilities(ctx(), oapi.GetCapabilitiesRequestObject{})
	require.NoError(t, err)
	okResp, ok := resp.(oapi.GetCapabilities200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	return oapi.Capabilities(okResp)
}

func runtimeNames(caps oapi.Capabilities) []string {
	names := make([]string, 0, len(caps.Runtimes))
	for _, rt := range caps.Runtimes {
		names = append(names, rt.Name)
	}
	return names
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

	caps := getCapabilities(t, svc)

	// Server identity and version serialization
	require.Equal(t, "testsha123", caps.Server.Version)
	require.Equal(t, apiVersion(), caps.Server.ApiVersion)
	require.NotEmpty(t, caps.Server.ApiVersion)
	require.NotEqual(t, "unknown", caps.Server.ApiVersion)

	// Host identity
	require.Equal(t, runtime.GOOS, caps.Host.Os)
	require.Equal(t, runtime.GOARCH, caps.Host.Arch)

	// Every host-supported runtime is reported, with availability from its
	// launch-prerequisite check and feature IDs derived from the registered
	// capability set — no handler-owned mapping.
	registered := hypervisor.RegisteredRuntimes()
	require.Len(t, caps.Runtimes, len(registered))
	for i, rt := range registered {
		require.Equal(t, string(rt.Type), caps.Runtimes[i].Name)
		require.Equal(t, rt.Available(), caps.Runtimes[i].Available,
			"runtime %s availability must come from the registry's launch check", rt.Type)
		require.Equal(t, rt.Capabilities.FeatureIDs(), caps.Runtimes[i].Features)
		require.NotNil(t, caps.Runtimes[i].Features, "features must serialize as [], not null")
	}

	// The configured default identity is retained. The test-service manager
	// defaults to cloud-hypervisor, which is available on Linux only (its
	// binaries are embedded, so registration implies launchability).
	require.Equal(t, string(hypervisor.TypeCloudHypervisor), caps.DefaultRuntime.Name)
	if runtime.GOOS == "linux" {
		require.True(t, caps.DefaultRuntime.Available)
		require.Contains(t, runtimeNames(caps), caps.DefaultRuntime.Name)
	} else {
		require.False(t, caps.DefaultRuntime.Available,
			"cloud-hypervisor default must not be reported available on non-Linux hosts")
		require.NotContains(t, runtimeNames(caps), caps.DefaultRuntime.Name)
	}

	// Network model and guest-visible gateway
	require.Equal(t, oapiNetworkModel(network.NetworkModel()), caps.Network.Model)
	require.NotNil(t, caps.Network.Gateway)
	require.Equal(t, "10.100.0.1", *caps.Network.Gateway)
	require.NotNil(t, caps.Network.Subnet)
	require.Equal(t, "10.100.0.0/16", *caps.Network.Subnet)
	require.False(t, caps.Network.GuestToGuest, "isolated default network must report guest_to_guest=false")

	// Image platforms always include the host-native guest platform
	require.Contains(t, caps.Images.Platforms, caps.Images.DefaultPlatform)

	// Server-level feature IDs are always present; per-runtime IDs never
	// appear at the server level.
	for _, f := range []string{"instances", "images", "builds", "volumes", "ingress", "exec", "logs"} {
		require.Contains(t, caps.Features, f)
	}
	require.NotContains(t, caps.Features, hypervisor.FeatureStandby)
	require.NotContains(t, caps.Features, hypervisor.FeatureSnapshots)
	if runtime.GOOS == "linux" {
		require.Contains(t, caps.Features, "devices")
	} else {
		require.NotContains(t, caps.Features, "devices")
	}
}

// TestGetCapabilitiesEachRuntimeIndependent proves runtimes report their own
// features independently: the runtimes on one host genuinely differ.
func TestGetCapabilitiesEachRuntimeIndependent(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	svc.NetworkManager = &stubCapabilitiesNetworkManager{}

	caps := getCapabilities(t, svc)

	features := make(map[string][]string, len(caps.Runtimes))
	for _, rt := range caps.Runtimes {
		features[rt.Name] = rt.Features
	}

	if runtime.GOOS != "linux" {
		t.Skipf("linux-only runtime matrix (GOOS=%s)", runtime.GOOS)
	}

	// cloud-hypervisor supports memory hotplug; firecracker and qemu do not.
	require.Contains(t, features["cloud-hypervisor"], hypervisor.FeatureHotplugMemory)
	require.NotContains(t, features["firecracker"], hypervisor.FeatureHotplugMemory)
	require.NotContains(t, features["qemu"], hypervisor.FeatureHotplugMemory)

	// firecracker has no GPU passthrough; qemu (standard board) does.
	require.NotContains(t, features["firecracker"], hypervisor.FeatureGPUPassthrough)
	require.Contains(t, features["qemu"], hypervisor.FeatureGPUPassthrough)

	// Lifecycle features hold for all Linux runtimes.
	for name, ids := range features {
		require.Contains(t, ids, hypervisor.FeatureSnapshots, "runtime %s", name)
		require.Contains(t, ids, hypervisor.FeatureStandby, "runtime %s", name)
		require.Contains(t, ids, hypervisor.FeatureVsock, "runtime %s", name)
	}

	// qemu-microvm appears automatically on amd64 and, unlike standard qemu,
	// must not advertise PCI passthrough.
	if runtime.GOARCH == "amd64" {
		require.Contains(t, features, "qemu-microvm")
		require.NotContains(t, features["qemu-microvm"], hypervisor.FeatureGPUPassthrough)
	} else {
		require.NotContains(t, features, "qemu-microvm")
	}
}

// TestGetCapabilitiesDefaultAvailabilityTracksLaunchCheck pins that the
// default runtime's availability is the same launch-prerequisite verdict as
// its runtimes[] entry — a registered default (e.g. qemu without a system
// binary) must not be reported available just because it is registered.
func TestGetCapabilitiesDefaultAvailabilityTracksLaunchCheck(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skipf("qemu registers on Linux only (GOOS=%s)", runtime.GOOS)
	}
	svc := newTestService(t)
	svc.NetworkManager = &stubCapabilitiesNetworkManager{}
	svc.InstanceManager = &stubDefaultRuntimeInstanceManager{defaultRuntime: hypervisor.TypeQEMU}

	var qemuAvailable, found bool
	for _, rt := range hypervisor.RegisteredRuntimes() {
		if rt.Type == hypervisor.TypeQEMU {
			qemuAvailable = rt.Available()
			found = true
		}
	}
	require.True(t, found, "qemu must be registered on Linux")

	caps := getCapabilities(t, svc)
	require.Equal(t, string(hypervisor.TypeQEMU), caps.DefaultRuntime.Name)
	require.Equal(t, qemuAvailable, caps.DefaultRuntime.Available,
		"a qemu default must report the launch-prerequisite verdict, not registration")
}

// TestGetCapabilitiesDefaultNotAvailable pins the contract when the configured
// default runtime cannot run on this host: the default identity is retained,
// available=false, and the available runtime list is unaffected.
func TestGetCapabilitiesDefaultNotAvailable(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	svc.NetworkManager = &stubCapabilitiesNetworkManager{}
	unavailable := hypervisor.TypeVZ
	if runtime.GOOS == "darwin" {
		unavailable = hypervisor.TypeFirecracker
	}
	svc.InstanceManager = &stubDefaultRuntimeInstanceManager{defaultRuntime: unavailable}

	caps := getCapabilities(t, svc)

	require.Equal(t, string(unavailable), caps.DefaultRuntime.Name)
	require.False(t, caps.DefaultRuntime.Available)
	require.NotContains(t, runtimeNames(caps), string(unavailable))
	require.Equal(t, len(hypervisor.RegisteredRuntimes()), len(caps.Runtimes),
		"an unavailable default must not hide the runtimes that are available")
}

// TestGetCapabilitiesDefaultWithoutAccessor pins the fallback contract for
// instance managers that do not expose the optional DefaultHypervisor
// accessor (it is deliberately not part of instances.Manager, so a wrapper
// embedding the interface hides the concrete manager's method): the handler
// must report the configured default — the value the wrapped manager was
// constructed from and launches still use — normalizing only an empty
// (unconfigured) value to the same cloud-hypervisor default lib/instances
// applies.
func TestGetCapabilitiesDefaultWithoutAccessor(t *testing.T) {
	t.Parallel()

	t.Run("configured non-default runtime is reported", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.Config.Hypervisor.Default = string(hypervisor.TypeFirecracker)
		svc.NetworkManager = &stubCapabilitiesNetworkManager{}
		svc.InstanceManager = &stubOpaqueInstanceManager{}

		caps := getCapabilities(t, svc)
		require.Equal(t, string(hypervisor.TypeFirecracker), caps.DefaultRuntime.Name,
			"a wrapped manager must not misreport the configured default as cloud-hypervisor")
	})

	t.Run("unconfigured default normalizes to cloud-hypervisor", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.NetworkManager = &stubCapabilitiesNetworkManager{}
		svc.InstanceManager = &stubOpaqueInstanceManager{}

		caps := getCapabilities(t, svc)
		require.Equal(t, string(hypervisor.TypeCloudHypervisor), caps.DefaultRuntime.Name)
	})

	t.Run("accessor overrides the configured value", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t)
		svc.Config.Hypervisor.Default = string(hypervisor.TypeFirecracker)
		svc.NetworkManager = &stubCapabilitiesNetworkManager{}
		svc.InstanceManager = &stubDefaultRuntimeInstanceManager{defaultRuntime: hypervisor.TypeQEMU}

		caps := getCapabilities(t, svc)
		require.Equal(t, string(hypervisor.TypeQEMU), caps.DefaultRuntime.Name,
			"the manager's effective default is authoritative when exposed")
	})
}

// TestGetCapabilitiesNoDefaultNetwork pins the gateway-absence contract: when
// no default network resolves, gateway and subnet are omitted rather than
// serialized as empty strings.
func TestGetCapabilitiesNoDefaultNetwork(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	svc.NetworkManager = &stubCapabilitiesNetworkManager{nw: nil}

	caps := getCapabilities(t, svc)

	require.Nil(t, caps.Network.Gateway)
	require.Nil(t, caps.Network.Subnet)
	require.False(t, caps.Network.GuestToGuest)
	require.Equal(t, oapiNetworkModel(network.NetworkModel()), caps.Network.Model)
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

func TestOapiNetworkModel(t *testing.T) {
	t.Parallel()
	require.Equal(t, oapi.Bridge, oapiNetworkModel(network.ModelBridge))
	require.Equal(t, oapi.Nat, oapiNetworkModel(network.ModelNAT))
}

func TestEmulationAvailable(t *testing.T) {
	t.Parallel()
	require.True(t, emulationAvailable("darwin", "arm64", true))
	require.False(t, emulationAvailable("darwin", "arm64", false),
		"an Apple Silicon host without Rosetta installed must not advertise emulation")
	require.False(t, emulationAvailable("darwin", "amd64", true))
	require.False(t, emulationAvailable("linux", "arm64", true))
	require.False(t, emulationAvailable("linux", "amd64", true))
}

func TestImagePlatforms(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"linux/amd64"}, imagePlatforms("amd64", false))
	require.Equal(t, []string{"linux/arm64"}, imagePlatforms("arm64", false))
	require.Equal(t,
		[]string{"linux/arm64", "linux/amd64"},
		imagePlatforms("arm64", true),
		"Apple Silicon macOS advertises Rosetta-emulated amd64")
}

func TestServerFeatures(t *testing.T) {
	t.Parallel()

	base := []string{"instances", "images", "builds", "volumes", "ingress", "exec", "logs"}

	require.Equal(t, append(base, "devices"), serverFeatures("linux", false))
	require.Equal(t, base, serverFeatures("darwin", false))
	require.Equal(t, append(base, "rosetta-emulation"), serverFeatures("darwin", true))
}

// TestAPIVersionMatchesSpec guards the version contract: the endpoint must
// serialize the same version as the embedded OpenAPI document.
func TestAPIVersionMatchesSpec(t *testing.T) {
	t.Parallel()
	spec, err := oapi.GetSwagger()
	require.NoError(t, err)
	require.Equal(t, spec.Info.Version, apiVersion())
}
