package system

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseKernelVersion(t *testing.T) {
	version, ok := ParseKernelVersion(string(Kernel_202603301))
	assert.True(t, ok)
	assert.Equal(t, Kernel_202603301, version)

	_, ok = ParseKernelVersion("ch-6.12.8-kernel-9.9-20990101")
	assert.False(t, ok)
}

func TestKernel202610301Registration(t *testing.T) {
	version, ok := ParseKernelVersion("ch-6.12.8-kernel-3.2-202610301")
	assert.True(t, ok)
	assert.Equal(t, Kernel_202610301, version)
	assert.Equal(t, Kernel_202610301, DefaultKernelVersion)
	assert.Equal(t, DefaultKernelVersion, SupportedKernelVersions[0])

	previous, ok := ParseKernelVersion(string(Kernel_202605291))
	assert.True(t, ok)
	assert.Equal(t, Kernel_202605291, previous)

	baseURL := "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-3.2-202610301/"
	assert.Equal(t, map[string]string{
		"x86_64":  baseURL + "vmlinux-x86_64",
		"aarch64": baseURL + "Image-arm64",
	}, KernelDownloadURLs[version])
	assert.Equal(t, map[string]string{
		"x86_64":  baseURL + "kernel-headers-x86_64.tar.gz",
		"aarch64": baseURL + "kernel-headers-aarch64.tar.gz",
	}, KernelHeaderURLs[version])
}
