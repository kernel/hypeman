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
	InitrdV1_0_0 InitrdVersion = "v1.0.0"
)

// InitrdBusyboxVersions maps initrd versions to specific busybox digests
// Using digest references (not mutable tags) ensures reproducible builds
// When bumping initrd version, you can reuse the same busybox digest if busybox doesn't need updating
var InitrdBusyboxVersions = map[InitrdVersion]string{
	InitrdV1_0_0: "docker.io/library/busybox@sha256:355b3a1bf5609da364166913878a8508d4ba30572d02020a97028c75477e24ff", // busybox:stable as of 2025-01-12
	// Add future versions here
}

var (
	// DefaultKernelVersion is the kernel version used for new instances
	DefaultKernelVersion = KernelCH_6_12_8_20250613

	// DefaultInitrdVersion is the initrd version used for new instances
	DefaultInitrdVersion = InitrdV1_0_0

	// SupportedKernelVersions lists all supported kernel versions
	SupportedKernelVersions = []KernelVersion{
		KernelCH_6_12_8_20250613,
		// Add future versions here
	}

	// SupportedInitrdVersions lists all supported initrd versions
	SupportedInitrdVersions = []InitrdVersion{
		InitrdV1_0_0,
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

