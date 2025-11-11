package system

import "runtime"

// KernelVersion represents a Cloud Hypervisor kernel version
type KernelVersion string

// InitrdVersion represents our internal initrd version
type InitrdVersion string

const (
	// Kernel versions from Cloud Hypervisor releases
	KernelV6_12_8 KernelVersion = "ch-v6.12.8"
	KernelV6_12_9 KernelVersion = "ch-v6.12.9"

	// Initrd versions (our internal versioning)
	// Bump when init script logic changes
	InitrdV1_0_0 InitrdVersion = "v1.0.0"
)

var (
	// DefaultKernelVersion is the kernel version used for new instances
	DefaultKernelVersion = KernelV6_12_9

	// DefaultInitrdVersion is the initrd version used for new instances
	DefaultInitrdVersion = InitrdV1_0_0

	// SupportedKernelVersions lists all supported kernel versions
	SupportedKernelVersions = []KernelVersion{
		KernelV6_12_8,
		KernelV6_12_9,
	}

	// SupportedInitrdVersions lists all supported initrd versions
	SupportedInitrdVersions = []InitrdVersion{
		InitrdV1_0_0,
	}
)

// KernelDownloadURLs maps kernel versions and architectures to download URLs
var KernelDownloadURLs = map[KernelVersion]map[string]string{
	KernelV6_12_8: {
		"x86_64":  "https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.8/vmlinux-x86_64",
		"aarch64": "https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.8/Image-aarch64",
	},
	KernelV6_12_9: {
		"x86_64":  "https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.9/vmlinux-x86_64",
		"aarch64": "https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.9/Image-aarch64",
	},
}

// GetArch returns the architecture string for the current platform
func GetArch() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		return "x86_64"
	}
	return arch
}

