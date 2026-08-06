//go:build linux

package devices

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverVGPUWithPropagatesMdevError(t *testing.T) {
	t.Parallel()

	discoveryErr := errors.New("mdev discovery failed")
	vendorCalled := false
	framework, vfs, err := discoverVGPUWith(
		func() ([]VirtualFunction, error) {
			return nil, discoveryErr
		},
		func() ([]VirtualFunction, error) {
			vendorCalled = true
			return []VirtualFunction{{PCIAddress: "0000:82:00.4"}}, nil
		},
	)

	require.ErrorIs(t, err, discoveryErr)
	assert.Equal(t, VGPUFrameworkNone, framework)
	assert.Nil(t, vfs)
	assert.False(t, vendorCalled)
}

func TestDiscoverVGPUWithFallsBackFromTypelessMdevBus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	busPath := filepath.Join(root, "sys", "class", "mdev_bus")
	require.NoError(t, os.MkdirAll(filepath.Join(busPath, "0000:82:00.4", "mdev_supported_types"), 0755))

	framework, vfs, err := discoverVGPUWith(
		func() ([]VirtualFunction, error) {
			return discoverMdevVFsWith(busPath, filepath.Join(root, "sys", "bus", "pci", "devices"), func() ([]MdevDevice, error) {
				return nil, nil
			})
		},
		func() ([]VirtualFunction, error) {
			return []VirtualFunction{{PCIAddress: "0000:82:00.4"}}, nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, VGPUFrameworkVendorVFIO, framework)
	assert.Equal(t, []VirtualFunction{{PCIAddress: "0000:82:00.4"}}, vfs)
}

func TestDiscoverVGPUWithPropagatesVendorVFIOError(t *testing.T) {
	t.Parallel()

	discoveryErr := errors.New("vendor VFIO discovery failed")
	framework, vfs, err := discoverVGPUWith(
		func() ([]VirtualFunction, error) {
			return nil, nil
		},
		func() ([]VirtualFunction, error) {
			return nil, discoveryErr
		},
	)

	require.ErrorIs(t, err, discoveryErr)
	assert.Equal(t, VGPUFrameworkNone, framework)
	assert.Nil(t, vfs)
}
