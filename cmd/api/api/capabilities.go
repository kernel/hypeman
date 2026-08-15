package api

import (
	"context"
	"runtime"
	"sync"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/oapi"
)

// Server-level feature IDs: API surfaces this server exposes regardless of
// which runtime backs an instance. Per-runtime feature IDs are owned by
// lib/hypervisor (hypervisor.Capabilities.FeatureIDs) so adding a runtime
// capability never requires touching this handler.
const (
	featureInstances        = "instances"
	featureImages           = "images"
	featureBuilds           = "builds"
	featureVolumes          = "volumes"
	featureIngress          = "ingress"
	featureExec             = "exec"
	featureLogs             = "logs"
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

	// The capability registry is platform-gated at registration time, so its
	// contents are exactly the runtimes this host can launch — including ones
	// added after this handler was written.
	registered := hypervisor.RegisteredRuntimes()
	runtimes := make([]oapi.CapabilitiesRuntime, 0, len(registered))
	defaultAvailable := false
	for _, rt := range registered {
		if rt.Type == defaultRuntime {
			defaultAvailable = true
		}
		runtimes = append(runtimes, oapi.CapabilitiesRuntime{
			Name:     string(rt.Type),
			Features: rt.Capabilities.FeatureIDs(),
		})
	}
	if !defaultAvailable {
		// Ordinary launches use the default runtime and will fail on this
		// host; surface that in logs as well as in the response.
		log.WarnContext(ctx, "configured default runtime is not available on this host",
			"runtime", string(defaultRuntime), "host_os", runtime.GOOS, "host_arch", runtime.GOARCH)
	}

	networkCaps, err := s.networkCapabilities(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to resolve network capabilities", "error", err)
		return oapi.GetCapabilities500JSONResponse{
			Code:    "internal_error",
			Message: "failed to resolve network capabilities",
		}, nil
	}

	emulation := emulationSupported(runtime.GOOS, runtime.GOARCH)

	resp := oapi.Capabilities{
		Server: oapi.CapabilitiesServer{
			Version:    s.Config.Version,
			ApiVersion: apiVersion(),
		},
		Host: oapi.CapabilitiesHost{
			Os:   runtime.GOOS,
			Arch: runtime.GOARCH,
		},
		DefaultRuntime: oapi.CapabilitiesDefaultRuntime{
			Name:      string(defaultRuntime),
			Available: defaultAvailable,
		},
		Runtimes: runtimes,
		Network:  *networkCaps,
		Images: oapi.CapabilitiesImages{
			Platforms:       imagePlatforms(runtime.GOARCH, emulation),
			DefaultPlatform: images.HostPlatformString(),
		},
		Features: serverFeatures(runtime.GOOS, emulation),
	}

	return oapi.GetCapabilities200JSONResponse(resp), nil
}

// networkCapabilities resolves the guest networking model and the
// guest-visible host gateway from the network manager's effective default
// network. Gateway and subnet are omitted (not serialized as empty strings)
// when no default network has been resolved.
func (s *ApiService) networkCapabilities(ctx context.Context) (*oapi.CapabilitiesNetwork, error) {
	caps := &oapi.CapabilitiesNetwork{
		Model:        oapiNetworkModel(network.NetworkModel()),
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
	if nw.Gateway != "" {
		gateway := nw.Gateway
		caps.Gateway = &gateway
	}
	if nw.Subnet != "" {
		subnet := nw.Subnet
		caps.Subnet = &subnet
	}
	caps.GuestToGuest = network.GuestToGuestEnabled(nw)
	return caps, nil
}

// oapiNetworkModel maps the network package's typed model onto the API enum,
// keeping lib/network free of oapi dependencies.
func oapiNetworkModel(m network.Model) oapi.CapabilitiesNetworkModel {
	switch m {
	case network.ModelBridge:
		return oapi.Bridge
	case network.ModelNAT:
		return oapi.Nat
	}
	// A new network.Model must also be added to the OpenAPI enum; surface it
	// verbatim rather than misreporting it as a known model.
	return oapi.CapabilitiesNetworkModel(m)
}

// emulationSupported reports whether the host can boot images built for the
// other CPU architecture. Only Apple Silicon macOS hosts qualify (vz with
// Rosetta). Rosetta installation itself is verified at instance start — a
// launch fails with installation guidance when it is missing — so this
// reports platform eligibility, and the OpenAPI description says so.
func emulationSupported(goos, goarch string) bool {
	return goos == "darwin" && goarch == "arm64"
}

// imagePlatforms returns the image platforms (os/arch) the host can run: the
// host-native Linux guest platform, plus Rosetta-emulated linux/amd64 on
// Apple Silicon.
func imagePlatforms(goarch string, emulation bool) []string {
	platforms := []string{"linux/" + goarch}
	if emulation {
		platforms = append(platforms, "linux/amd64")
	}
	return platforms
}

// serverFeatures builds the server-level feature ID list: API surfaces that
// are always present plus host-platform conditionals.
func serverFeatures(goos string, emulation bool) []string {
	features := []string{
		featureInstances,
		featureImages,
		featureBuilds,
		featureVolumes,
		featureIngress,
		featureExec,
		featureLogs,
	}
	// Device (GPU/PCI) passthrough management is only available on Linux hosts.
	if goos == "linux" {
		features = append(features, featureDevices)
	}
	if emulation {
		features = append(features, featureRosettaEmulation)
	}
	return features
}
