package system

import "runtime"

// KernelVersion represents a Cloud Hypervisor kernel version
type KernelVersion string

const (
	// Kernel versions from Kernel linux build
	Kernel_202511182 KernelVersion = "ch-6.12.8-kernel-1-202511182"
)

var (
	// DefaultKernelVersion is the kernel version used for new instances
	DefaultKernelVersion = Kernel_202511182

	// SupportedKernelVersions lists all supported kernel versions
	SupportedKernelVersions = []KernelVersion{
		Kernel_202511182,
		// Add future versions here
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

