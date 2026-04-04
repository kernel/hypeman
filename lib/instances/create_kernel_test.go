package instances

import (
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveCreateKernelVersionUsesDefaultWithoutLabel(t *testing.T) {
	defaultKernel := system.Kernel_202603091

	got, err := resolveCreateKernelVersion(&images.Image{Name: "docker.io/library/alpine:latest"}, defaultKernel)
	require.NoError(t, err)
	assert.Equal(t, defaultKernel, got)
}

func TestResolveCreateKernelVersionUsesImageLabel(t *testing.T) {
	defaultKernel := system.Kernel_202603091
	imageInfo := &images.Image{
		Name: "docker.io/onkernel/chromium-headful-vgpu:test",
		Labels: map[string]string{
			system.ImageKernelVersionLabel: string(system.Kernel_202603301),
		},
	}

	got, err := resolveCreateKernelVersion(imageInfo, defaultKernel)
	require.NoError(t, err)
	assert.Equal(t, system.Kernel_202603301, got)
}

func TestResolveCreateKernelVersionRejectsUnknownLabel(t *testing.T) {
	defaultKernel := system.Kernel_202603091
	imageInfo := &images.Image{
		Name: "docker.io/onkernel/chromium-headful-vgpu:test",
		Labels: map[string]string{
			system.ImageKernelVersionLabel: "ch-6.12.8-kernel-9.9-20990101",
		},
	}

	_, err := resolveCreateKernelVersion(imageInfo, defaultKernel)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
	assert.Contains(t, err.Error(), system.ImageKernelVersionLabel)
}
