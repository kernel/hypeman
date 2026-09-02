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

func TestDiscoverMdevVFsSkipsUnreadableVF(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	busPath := filepath.Join(root, "sys", "class", "mdev_bus")
	pciPath := filepath.Join(root, "sys", "bus", "pci", "devices")
	require.NoError(t, os.MkdirAll(filepath.Join(busPath, "0000:82:00.4", "mdev_supported_types", "nvidia-556"), 0755))
	// A regular file makes the supported-types read fail without IsNotExist.
	require.NoError(t, os.MkdirAll(filepath.Join(busPath, "0000:82:00.5"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(busPath, "0000:82:00.5", "mdev_supported_types"), nil, 0644))

	vfs, err := discoverMdevVFsWith(busPath, pciPath, func() ([]MdevDevice, error) {
		return nil, nil
	})
	require.NoError(t, err)
	require.Len(t, vfs, 1)
	assert.Equal(t, "0000:82:00.4", vfs[0].PCIAddress)
}

func TestDiscoverMdevVFsFailsWhenNoVFIsReadable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	busPath := filepath.Join(root, "sys", "class", "mdev_bus")
	pciPath := filepath.Join(root, "sys", "bus", "pci", "devices")
	require.NoError(t, os.MkdirAll(filepath.Join(busPath, "0000:82:00.4"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(busPath, "0000:82:00.4", "mdev_supported_types"), nil, 0644))

	// With every VF unreadable, discovery must fail rather than report an
	// empty inventory that would demote the host to vendor VFIO or
	// passthrough.
	_, err := discoverMdevVFsWith(busPath, pciPath, func() ([]MdevDevice, error) {
		return nil, nil
	})
	require.Error(t, err)
}
