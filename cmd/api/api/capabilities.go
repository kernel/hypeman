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

// defaultHypervisorProvider is the narrow accessor this handler needs from
// the instance manager. The concrete instances manager implements it; it is
// type-asserted rather than added to instances.Manager so alternate Manager
// implementations (mocks, wrappers) compiled against the public module keep
// building without a new method.
type defaultHypervisorProvider interface {
	DefaultHypervisor() hypervisor.Type
}

// GetCapabilities reports host, runtime, network, and image capabilities.
func (s *ApiService) GetCapabilities(ctx context.Context, _ oapi.GetCapabilitiesRequestObject) (oapi.GetCapabilitiesResponseObject, error) {
	log := logger.FromContext(ctx)

	// Resolve the default runtime the way launches do. Prefer the manager's
	// own effective default via the optional accessor; when the manager does
	// not expose it (a wrapper embedding instances.Manager hides the concrete
	// manager's extra method), fall back to the configured default the manager
	// was constructed from — launches still route through the wrapped manager,
	// so a hardcoded fallback would misreport a Firecracker/QEMU default as
	// cloud-hypervisor. Only an empty (unconfigured) value normalizes to the
	// compile-time default, mirroring lib/instances.NewManagerWithConfigE.
	defaultRuntime := hypervisor.Type(s.Config.Hypervisor.Default)
	if defaultRuntime == "" {
		defaultRuntime = hypervisor.TypeCloudHypervisor
	}
	if p, ok := s.InstanceManager.(defaultHypervisorProvider); ok {
		defaultRuntime = p.DefaultHypervisor()
	}

	// The capability registry is platform-gated at registration time, so its
	// contents are exactly the runtimes this build supports on this host —
	// including ones added after this handler was written. Capabilities are
	// resolved per request so configuration applied after init (for example a
	// pinned cloud-hypervisor version) remains visible. Backends may cache host
	// launch-readiness probes when those prerequisites are startup state.
	registered := hypervisor.RegisteredRuntimes()
	runtimes := make([]oapi.CapabilitiesRuntime, 0, len(registered))
	defaultRegistered := false
	defaultAvailable := false
	for _, rt := range registered {
		available := rt.Available()
		if rt.Type == defaultRuntime {
			defaultRegistered = true
			defaultAvailable = available
		}
		if !available {
			log.WarnContext(ctx, "runtime is registered but missing launch prerequisites",
				"runtime", string(rt.Type), "error", rt.LaunchErr)
		}
		runtimes = append(runtimes, oapi.CapabilitiesRuntime{
			Name:      string(rt.Type),
			Available: available,
			Features:  rt.Capabilities.FeatureIDs(),
		})
	}
	if !defaultAvailable {
		// A registered default is supported on this platform but missing a
		// prerequisite, so ordinary launches will fail and operators need to
		// act. An unregistered default simply cannot run on this platform; log
		// that expected configuration mismatch at info level.
		if defaultRegistered {
			log.WarnContext(ctx, "configured default runtime is missing launch prerequisites",
				"runtime", string(defaultRuntime), "host_os", runtime.GOOS, "host_arch", runtime.GOARCH)
		} else {
			log.InfoContext(ctx, "configured default runtime cannot run on this platform",
				"runtime", string(defaultRuntime), "host_os", runtime.GOOS, "host_arch", runtime.GOARCH)
		}
	}

	networkCaps, err := s.networkCapabilities(ctx)
	if err != nil {
		log.ErrorContext(ctx, "failed to resolve network capabilities", "error", err)
		return oapi.GetCapabilities500JSONResponse{
			Code:    "internal_error",
			Message: "failed to resolve network capabilities",
		}, nil
	}

	emulation := emulationAvailable(runtime.GOOS, runtime.GOARCH, rosettaInstalled())

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

// emulationAvailable reports whether the host can boot images built for the
// other CPU architecture right now. Only Apple Silicon macOS hosts qualify
// (vz with Rosetta), and only when the Rosetta availability probe — the same
// Virtualization.framework check the vz-shim enforces at launch — reports it
// installed. Platform eligibility alone (darwin/arm64) is deliberately not
// enough: a macOS 11/12 host or one without Rosetta installed would advertise
// launches that lib/hypervisor/vz rejects.
func emulationAvailable(goos, goarch string, rosettaInstalled bool) bool {
	return goos == "darwin" && goarch == "arm64" && rosettaInstalled
}

// imagePlatforms returns the image platforms (os/arch) the host can run: the
// host-native Linux guest platform, plus Rosetta-emulated linux/amd64 on
// Apple Silicon with Rosetta installed.
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
