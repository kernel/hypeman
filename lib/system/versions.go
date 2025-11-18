package system

import "runtime"

// KernelVersion represents a Cloud Hypervisor kernel version
type KernelVersion string

// InitrdVersion represents our internal initrd version
type InitrdVersion string

const (
	// Kernel versions from Kernel linux build
	Kernel_202511182 KernelVersion = "ch-6.12.8-kernel-1-202511182"

	// Initrd versions (our internal versioning)
	// Bump when init script logic changes
	InitrdV2_0_0 InitrdVersion = "v2.0.0"
	InitrdV2_0_1 InitrdVersion = "v2.0.1"
	InitrdV2_0_2 InitrdVersion = "v2.0.2"
)

// InitrdBaseImages maps initrd versions to specific base image references
// v2.0.0: Uses pre-built Alpine image with exec-agent from Docker Hub (multi-arch OCI manifest list)
// v2.0.1: Uses same base but we will inject local agent
// v2.0.2: Uses same base but with interactive shell fallback
// v2.0.2-dev: Local dev build (built via cmd/build-dev-initrd)
var InitrdBaseImages = map[InitrdVersion]string{
	InitrdV2_0_0:                "docker.io/onkernel/hypeman-initrd:d0e84c2-oci",
	InitrdV2_0_1:                "docker.io/onkernel/hypeman-initrd:d0e84c2-oci",
	InitrdV2_0_2:                "docker.io/onkernel/hypeman-initrd:d0e84c2-oci",
	InitrdVersion("v2.0.2-dev"): "docker.io/onkernel/hypeman-initrd:d0e84c2-oci", // Not used, already built locally
	// Add future versions here
}

var (
	// DefaultKernelVersion is the kernel version used for new instances
	DefaultKernelVersion = Kernel_202511182

	// DefaultInitrdVersion is the initrd version used for new instances
	DefaultInitrdVersion = InitrdVersion("v2.0.2-dev")

	// SupportedKernelVersions lists all supported kernel versions
	SupportedKernelVersions = []KernelVersion{
		Kernel_202511182,
		// Add future versions here
	}

	// SupportedInitrdVersions lists all supported initrd versions
	SupportedInitrdVersions = []InitrdVersion{
		InitrdV2_0_0,
		InitrdV2_0_1,
		InitrdV2_0_2,
	}
)

// KernelDownloadURLs maps kernel versions and architectures to download URLs
var KernelDownloadURLs = map[KernelVersion]map[string]string{
	Kernel_202511182: {
		"x86_64":  "https://github.com/onkernel/linux/releases/download/ch-6.12.8-kernel-1-202511182/vmlinux-x86_64",
		"aarch64": "https://github.com/onkernel/linux/releases/download/ch-6.12.8-kernel-1-202511182/Image-arm64",
	},
	// Add future versions here
}

// GetArch returns the architecture string for the current platform
func GetArch() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		return "x86_64"
	}
	return arch
}

