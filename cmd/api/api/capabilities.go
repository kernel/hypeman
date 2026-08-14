package api

import (
	"context"
	"runtime"
	"slices"
	"sync"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/oapi"
)

// Stable feature IDs reported by the capabilities endpoint. Clients gate
// behavior on these (and the structured runtime booleans) rather than on
// hypervisor names.
const (
	featureInstances        = "instances"
	featureImages           = "images"
	featureBuilds           = "builds"
	featureVolumes          = "volumes"
	featureIngress          = "ingress"
	featureExec             = "exec"
	featureLogs             = "logs"
	featureStandby          = "standby"
	featureSnapshots        = "snapshots"
	featureFork             = "fork"
	featurePause            = "pause"
	featureHotplugMemory    = "hotplug-memory"
	featureBalloonControl   = "balloon-control"
	featureVsock            = "vsock"
	featureGPUPassthrough   = "gpu-passthrough"
	featureDiskIOLimit      = "disk-io-limit"
	featureDiskResize       = "disk-resize"
	featureDevices          = "devices"
	featureRosettaEmulation = "rosetta-emulation"
)

// apiVersion is the API contract version from the embedded OpenAPI document.
// The decoded spec is cached: decoding it per request is needlessly expensive.
var apiVersion = sync.OnceValue(func() string {
	spec, err := oapi.GetSwagger()
	if err != nil || spec.Info == nil {
		return "unknown"
	}
	return spec.Info.Version
})

// GetCapabilities reports host, runtime, network, and image capabilities.
func (s *ApiService) GetCapabilities(ctx context.Context, _ oapi.GetCapabilitiesRequestObject) (oapi.GetCapabilitiesResponseObject, error) {
	log := logger.FromContext(ctx)

	defaultRuntime := hypervisor.TypeCloudHypervisor
	if s.InstanceManager != nil {
		defaultRuntime = s.InstanceManager.DefaultHypervisor()
	}
	supported := supportedRuntimes(runtime.GOOS)
	caps, capsKnown := hypervisor.CapabilitiesForType(defaultRuntime)
	if capsKnown && !slices.Contains(supported, string(defaultRuntime)) {
		// The configured default runtime cannot run on this host platform;
		// advertising its features would overstate support.
		capsKnown = false
	}
	if !capsKnown {
		// Report zeroed features rather than guessing.
		log.WarnContext(ctx, "default runtime is not usable on this host platform",
			"runtime", string(defaultRuntime), "host_os", runtime.GOOS)
		caps = hypervisor.Capabilities{}
	}

	emulation := emulationSupported(runtime.GOOS, runtime.GOARCH, supported)

	networkCaps, err := s.networkCapabilities(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to resolve network capabilities", "error", err)
		return oapi.GetCapabilities500JSONResponse{
			Code:    "internal_error",
			Message: "failed to resolve network capabilities",
		}, nil
	}

	resp := oapi.Capabilities{
		Server: oapi.CapabilitiesServer{
			Version:    s.Config.Version,
			ApiVersion: apiVersion(),
		},
		Host: oapi.CapabilitiesHost{
			Os:   runtime.GOOS,
			Arch: runtime.GOARCH,
		},
		Runtime: oapi.CapabilitiesRuntime{
			Default:        string(defaultRuntime),
			Supported:      supported,
			Snapshot:       caps.SupportsSnapshot,
			Standby:        standbySupported(caps),
			Pause:          caps.SupportsPause,
			HotplugMemory:  caps.SupportsHotplugMemory,
			BalloonControl: caps.SupportsBalloonControl,
			Vsock:          caps.SupportsVsock,
			GpuPassthrough: caps.SupportsGPUPassthrough,
			DiskIoLimit:    caps.SupportsDiskIOLimit,
			DiskResize:     caps.SupportsDiskResize,
		},
		Network: *networkCaps,
		Images: oapi.CapabilitiesImages{
			Platforms:       imagePlatforms(runtime.GOARCH, emulation),
			DefaultPlatform: images.HostPlatformString(),
		},
		Features: assembleFeatures(runtime.GOOS, caps, emulation),
	}

	return oapi.GetCapabilities200JSONResponse(resp), nil
}

// networkCapabilities resolves the guest networking model and the
// guest-visible host gateway from the network manager's effective default
// network.
func (s *ApiService) networkCapabilities(ctx context.Context) (*oapi.CapabilitiesNetwork, error) {
	caps := &oapi.CapabilitiesNetwork{
		Model:        oapi.CapabilitiesNetworkModel(network.NetworkModel()),
		GuestToGuest: false,
	}
	if s.NetworkManager == nil {
		return caps, nil
	}
	nw, err := s.NetworkManager.EffectiveDefaultNetwork()
	if err != nil {
		return nil, err
	}
	if nw == nil {
		return caps, nil
	}
	caps.Gateway = nw.Gateway
	if nw.Subnet != "" {
		subnet := nw.Subnet
		caps.Subnet = &subnet
	}
	caps.GuestToGuest = network.GuestToGuestEnabled(nw)
	return caps, nil
}

// standbySupported reports whether standby (pause + memory snapshot) and
// later restore are supported. Standby requires both snapshot and pause.
func standbySupported(caps hypervisor.Capabilities) bool {
	return caps.SupportsSnapshot && caps.SupportsPause
}

// supportedRuntimes returns the runtime identifiers usable on a host OS.
// This is a platform floor, not a registry listing: a runtime whose package
// registered capabilities but that cannot run on the host (e.g. firecracker
// on macOS) is not listed.
func supportedRuntimes(goos string) []string {
	switch goos {
	case "darwin":
		return []string{string(hypervisor.TypeVZ)}
	default:
		return []string{
			string(hypervisor.TypeCloudHypervisor),
			string(hypervisor.TypeFirecracker),
			string(hypervisor.TypeQEMU),
		}
	}
}

// emulationSupported reports whether the host can boot images built for the
// other CPU architecture. This is a host capability rather than a property of
// the configured default: vz can use Rosetta on Apple Silicon whenever it is
// available on the host.
func emulationSupported(goos, goarch string, supported []string) bool {
	return goos == "darwin" &&
		goarch == "arm64" &&
		slices.Contains(supported, string(hypervisor.TypeVZ))
}

// imagePlatforms returns the image platforms (os/arch) the host can run:
// the host-native platform plus the emulated one when available.
func imagePlatforms(goarch string, emulation bool) []string {
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	platforms := []string{"linux/" + goarch}
	if emulation {
		switch goarch {
		case "arm64":
			platforms = append(platforms, "linux/amd64")
		case "amd64":
			platforms = append(platforms, "linux/arm64")
		}
	}
	return platforms
}

// assembleFeatures builds the stable feature ID list: always-present base API
// surfaces plus conditional entries derived from the effective default
// runtime's capabilities and the host platform.
func assembleFeatures(goos string, caps hypervisor.Capabilities, emulation bool) []string {
	features := []string{
		featureInstances,
		featureImages,
		featureBuilds,
		featureVolumes,
		featureIngress,
		featureExec,
		featureLogs,
	}
	if standbySupported(caps) {
		features = append(features, featureStandby)
	}
	if caps.SupportsSnapshot {
		features = append(features, featureSnapshots, featureFork)
	}
	if caps.SupportsPause {
		features = append(features, featurePause)
	}
	if caps.SupportsHotplugMemory {
		features = append(features, featureHotplugMemory)
	}
	if caps.SupportsBalloonControl {
		features = append(features, featureBalloonControl)
	}
	if caps.SupportsVsock {
		features = append(features, featureVsock)
	}
	if caps.SupportsGPUPassthrough {
		features = append(features, featureGPUPassthrough)
	}
	if caps.SupportsDiskIOLimit {
		features = append(features, featureDiskIOLimit)
	}
	if caps.SupportsDiskResize {
		features = append(features, featureDiskResize)
	}
	// Device passthrough (GPU/PCI) is only meaningful on Linux hosts.
	if goos == "linux" {
		features = append(features, featureDevices)
	}
	if emulation {
		features = append(features, featureRosettaEmulation)
	}
	return features
}
