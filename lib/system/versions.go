package system

import "runtime"

// KernelVersion represents a Cloud Hypervisor kernel version
type KernelVersion string

// InitrdVersion represents our internal initrd version
type InitrdVersion string

const (
	// Kernel versions from Cloud Hypervisor releases (full version with date)
	KernelCH_6_12_8_20250613 KernelVersion = "ch-release-v6.12.8-20250613"

	// Initrd versions (our internal versioning)
	// Bump when init script logic changes
	InitrdV2_0_0 InitrdVersion = "v2.0.0"
)

// InitrdBaseImages maps initrd versions to specific base image references
// v2.0.0: Uses pre-built Alpine image with exec-agent from Docker Hub (multi-arch OCI manifest list)
var InitrdBaseImages = map[InitrdVersion]string{
	InitrdV2_0_0: "docker.io/onkernel/hypeman-initrd:1d4efc9-oci",
	// Add future versions here
}

var (
	// DefaultKernelVersion is the kernel version used for new instances
	DefaultKernelVersion = KernelCH_6_12_8_20250613

	// DefaultInitrdVersion is the initrd version used for new instances
	DefaultInitrdVersion = InitrdV2_0_0

	// SupportedKernelVersions lists all supported kernel versions
	SupportedKernelVersions = []KernelVersion{
		KernelCH_6_12_8_20250613,
		// Add future versions here
	}

	// SupportedInitrdVersions lists all supported initrd versions
	SupportedInitrdVersions = []InitrdVersion{
		InitrdV2_0_0,
	}
)

// KernelDownloadURLs maps kernel versions and architectures to download URLs
var KernelDownloadURLs = map[KernelVersion]map[string]string{
	KernelCH_6_12_8_20250613: {
		"x86_64":  "https://github.com/cloud-hypervisor/linux/releases/download/ch-release-v6.12.8-20250613/vmlinux-x86_64",
		"aarch64": "https://github.com/cloud-hypervisor/linux/releases/download/ch-release-v6.12.8-20250613/Image-aarch64",
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

