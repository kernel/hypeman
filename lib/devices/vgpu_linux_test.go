//go:build linux

package devices

import (
	"errors"
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
