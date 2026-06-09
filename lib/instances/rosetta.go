package instances

import (
	"fmt"
	"runtime"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// deriveEnableRosetta decides whether to attach the Rosetta share for an
// instance. When the image needs emulation, Rosetta is enabled automatically on
// vz/Apple-silicon hosts (mirroring Docker Desktop, where Rosetta is a host
// setting rather than a per-container flag). On any other host an emulated
// image cannot boot, so the create is rejected with an emulation-agnostic
// error. It is a pure function of the resolved hypervisor and host so it can be
// unit-tested without booting a VM.
func deriveEnableRosetta(imageNeedsEmulation bool, hvType hypervisor.Type) (bool, error) {
	if !imageNeedsEmulation {
		return false, nil
	}
	if hvType == hypervisor.TypeVZ && runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return true, nil
	}
	return false, fmt.Errorf("%w: running %s images requires an emulation-capable host; this host is %s/%s",
		ErrInvalidRequest, emulatedArchName(), runtime.GOOS, runtime.GOARCH)
}

// emulatedArchName names the architecture an emulation-capable host would run.
// On arm64 that is amd64 and vice-versa.
func emulatedArchName() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

// hostOSArchString renders the host OS/arch (e.g. "darwin/arm64") for
// log/error context. This is the host-kernel identity and is distinct from
// images.HostPlatformString, which reports the guest host platform
// ("linux/<arch>") used to resolve and match image manifests.
func hostOSArchString() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
