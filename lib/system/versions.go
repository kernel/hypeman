package system

import "runtime"

// KernelVersion represents a Cloud Hypervisor kernel version
type KernelVersion string

const (
	// Kernel_202601152 is the current kernel version with vGPU support
	Kernel_202601152 KernelVersion = "ch-6.12.8-kernel-1.3-202601152"
)

var (
	// DefaultKernelVersion is the kernel version used for new instances
	DefaultKernelVersion = Kernel_202601152

	// SupportedKernelVersions lists all supported kernel versions
	SupportedKernelVersions = []KernelVersion{
		Kernel_202601152,
	}
)

// KernelDownloadURLs maps kernel versions and architectures to download URLs
var KernelDownloadURLs = map[KernelVersion]map[string]string{
	Kernel_202601152: {
		"x86_64":  "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.3-202601152/vmlinux-x86_64",
		"aarch64": "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.3-202601152/Image-arm64",
	},
}

// KernelHeaderURLs maps kernel versions and architectures to kernel header tarball URLs
// These tarballs contain kernel headers needed for DKMS to build out-of-tree modules (e.g., NVIDIA vGPU drivers)
var KernelHeaderURLs = map[KernelVersion]map[string]string{
	Kernel_202601152: {
		"x86_64":  "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.3-202601152/kernel-headers-x86_64.tar.gz",
		"aarch64": "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.3-202601152/kernel-headers-aarch64.tar.gz",
	},
}

// GetArch returns the architecture string for the current platform
func GetArch() string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		return "x86_64"
	}
	if arch == "arm64" {
		return "aarch64"
	}
	return arch
}
